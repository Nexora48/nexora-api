// Package app wires every module into one chi router (used by main and tests).
package app

import (
	"context"
	"github.com/Nexora48/nexora-api/internal/amenity"
	"github.com/Nexora48/nexora-api/internal/attendance"
	"github.com/Nexora48/nexora-api/internal/billing"
	"github.com/Nexora48/nexora-api/internal/helper"
	"github.com/Nexora48/nexora-api/internal/payment"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/blob"
	"github.com/Nexora48/nexora-api/internal/complaint"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/contacts"
	"github.com/Nexora48/nexora-api/internal/delivery"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/gate"
	"github.com/Nexora48/nexora-api/internal/health"
	"github.com/Nexora48/nexora-api/internal/household"
	"github.com/Nexora48/nexora-api/internal/live"
	"github.com/Nexora48/nexora-api/internal/middleware"
	"github.com/Nexora48/nexora-api/internal/notice"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/poll"
	"github.com/Nexora48/nexora-api/internal/rbac"
	"github.com/Nexora48/nexora-api/internal/realtime"
	"github.com/Nexora48/nexora-api/internal/resident"
	"github.com/Nexora48/nexora-api/internal/society"
	"github.com/Nexora48/nexora-api/internal/sos"
	"github.com/Nexora48/nexora-api/internal/superadmin"
	"github.com/Nexora48/nexora-api/internal/user"
	"github.com/Nexora48/nexora-api/internal/visitor"
)

type Deps struct {
	Cfg   config.Config
	Log   *slog.Logger
	Pool  *pgxpool.Pool
	Redis *redis.Client
	SMS   sms.Sender
	// Push overrides the push sender (tests); nil selects FCM when configured, else the log sender.
	Push notify.Sender
}

// App exposes the router plus the middleware factory modules use to guard routes.
type App struct {
	Router *chi.Mux
	// Protect returns the standard chain: auth → tenant → permission? → feature?.
	// Empty perm / feature skip that check.
	Protect func(perm, featureKey string) chi.Middlewares
	// Visitors is exposed so the process can run its expiry sweeper.
	Visitors   *visitor.Service
	Hub        *realtime.Hub
	Sos        *sos.Service
	Notices    *notice.Service
	Complaints *complaint.Service
	Polls      *poll.Service
	Amenities  *amenity.Service
	Attendance *attendance.Service
	Billing    *billing.Service
	Payments   *payment.Service
	Notify     *notify.Service
	gates      *gate.Service
}

// Start runs the background jobs (visit expiry, shift cleanup, cross-instance event bridge) until ctx ends.
func (a *App) Start(ctx context.Context) {
	go a.Visitors.RunSweeper(ctx, time.Minute)
	go a.gates.RunSweeper(ctx, 10*time.Minute)
	go a.Hub.Run(ctx)
	go a.Notify.RunWorker(ctx, time.Minute)
	go a.Sos.RunSweeper(ctx, 15*time.Second)
	go a.Notices.RunSweeper(ctx, time.Minute)
	go a.Complaints.RunSweeper(ctx, time.Hour)
	go a.Polls.RunSweeper(ctx, time.Minute)
	go a.Amenities.RunSweeper(ctx, time.Minute)
	go a.Attendance.RunSweeper(ctx, 10*time.Minute)
	go a.Billing.RunSweeper(ctx, time.Hour)
	go a.Payments.RunSweeper(ctx, time.Minute)
}

func New(d Deps) *App {
	tokens := auth.NewTokenManager(d.Cfg.JWTSecret, d.Cfg.AccessTTL)
	users := user.NewRepository(d.Pool)
	societies := society.NewRepository(d.Pool)
	rb := rbac.New(d.Pool)
	feats := feature.New(d.Pool)
	aud := audit.New(d.Pool)
	otp := auth.NewOTPService(d.Redis, d.SMS, d.Cfg.JWTSecret, d.Cfg.OTPTTL, d.Cfg.IsDev())
	authSvc := auth.NewService(auth.NewRepository(d.Pool), users, societies, tokens, otp, d.Redis, aud, d.Cfg.RefreshTTL)
	authH := auth.NewHandler(authSvc)
	meH := user.NewHandler(users, societies, rb, feats)
	societyH := society.NewHandler(societies, aud)
	residentH := resident.NewHandler(resident.NewService(d.Pool, aud))
	householdH := household.NewHandler(household.NewService(d.Pool, aud))
	visitorSvc := visitor.NewService(d.Pool, d.Redis, aud, d.Cfg.JWTSecret, d.Cfg.VisitorMaxWindow, d.Cfg.VisitorWalkinWait)
	visitorH := visitor.NewHandler(visitorSvc)
	hub := realtime.NewHub(d.Redis)
	var sender notify.Sender = notify.LogSender{Log: d.Log}
	if d.Push != nil {
		sender = d.Push
	} else if d.Cfg.FCMCredentialsFile != "" {
		if f, err := notify.NewFCM(d.Cfg.FCMProjectID, d.Cfg.FCMCredentialsFile); err == nil {
			sender = f
		} else {
			d.Log.Error("fcm disabled", "err", err)
		}
	}
	noteSvc := notify.NewService(d.Pool, sender, d.Log)
	noteSvc.SetPublisher(hub)
	if d.Cfg.NotifyRetention > 0 {
		noteSvc.SetRetention(d.Cfg.NotifyRetention)
	}
	dispatcher := live.NewDispatcher(hub, noteSvc, d.Pool, d.Cfg.VisitorWalkinWait)
	visitorSvc.SetEvents(dispatcher)
	deliverySvc := delivery.NewService(d.Pool, aud, feats)
	visitorSvc.SetRules(deliverySvc)
	deliveryH := delivery.NewHandler(deliverySvc)
	noteH := notify.NewHandler(noteSvc)
	sosSvc := sos.NewService(d.Pool, aud, noteSvc, hub, d.Cfg.SOSEscalateAfter, d.Cfg.SOSCancelWindow)
	sosH := sos.NewHandler(sosSvc)
	var store blob.Store // the same store as visitor photos; nil disables uploads
	if d.Cfg.BlobPath != "" {
		if st, err := blob.NewDisk(d.Cfg.BlobPath); err == nil {
			store = st
		}
	}
	files := attachment.NewService(d.Pool, store, d.Cfg.AttachmentMaxMB)
	noticeSvc := notice.NewService(d.Pool, aud, files, noteSvc, hub)
	noticeH := notice.NewHandler(noticeSvc, files)
	complaintSvc := complaint.NewService(d.Pool, aud, files, noteSvc, hub, d.Cfg.ComplaintReopen, d.Cfg.ComplaintAutoclose)
	complaintH := complaint.NewHandler(complaintSvc, files)
	pollSvc := poll.NewService(d.Pool, aud, noteSvc, hub)
	pollH := poll.NewHandler(pollSvc)
	contactsSvc := contacts.NewService(d.Pool, aud)
	contactsH := contacts.NewHandler(contactsSvc)
	if d.Cfg.BlobPath != "" {
		if store, err := blob.NewDisk(d.Cfg.BlobPath); err == nil {
			visitorSvc.SetBlobs(store)
		} else {
			d.Log.Error("photo storage disabled", "err", err)
		}
	}
	amenitySvc := amenity.NewService(d.Pool, aud, files, noteSvc, hub, d.Cfg.AmenityReminder)
	amenityH := amenity.NewHandler(amenitySvc, files)
	helperSvc := helper.NewService(d.Pool, aud, files, noteSvc, hub, d.Cfg.JWTSecret)
	helperH := helper.NewHandler(helperSvc, files)
	attendSvc := attendance.NewService(d.Pool, aud, files, noteSvc, hub, d.Cfg.JWTSecret)
	attendH := attendance.NewHandler(attendSvc, files)
	billSvc := billing.NewService(d.Pool, aud, noteSvc, hub)
	billH := billing.NewHandler(billSvc)
	cipherKey := d.Cfg.PlatformEncKey
	if cipherKey == "" {
		cipherKey = "gateway:" + d.Cfg.JWTSecret
	}
	secretBox, err := superadmin.NewCipher(cipherKey)
	if err != nil {
		panic(err)
	}
	paySvc := payment.NewService(d.Pool, billSvc, aud, noteSvc, secretBox, d.Cfg.JWTSecret, d.Cfg.PaymentFake)
	payH := payment.NewHandler(paySvc, d.Cfg.PublicBaseURL)
	gateSvc := gate.NewService(d.Pool, aud, hub)
	gateSvc.SetAttendance(attendSvc, feats)
	gateH := gate.NewHandler(gateSvc, visitorSvc)
	wsH := realtime.NewHandler(hub, tokens, d.Pool, d.Cfg.CORSOrigins)
	need := func(perm string) func(http.Handler) http.Handler { return middleware.RequirePermission(rb, perm) }

	protect := func(perm, featureKey string) chi.Middlewares {
		chain := chi.Middlewares{middleware.Auth(tokens), middleware.Tenant(societies), middleware.RequirePasswordChanged}
		if perm != "" {
			chain = append(chain, middleware.RequirePermission(rb, perm))
		}
		if featureKey != "" {
			chain = append(chain, middleware.RequireFeature(feats, featureKey))
		}
		return chain
	}

	hl := health.New(map[string]health.Pinger{
		"postgres": d.Pool.Ping,
		"redis":    func(c context.Context) error { return d.Redis.Ping(c).Err() },
	})

	// Internal Nexora console (platform staff, separate tokens and secret)
	var saSvc *superadmin.Service
	if cipher, err := superadmin.NewCipher(d.Cfg.PlatformEncKey); err == nil && d.Cfg.PlatformJWTSecret != "" {
		saSvc = superadmin.NewService(d.Pool, superadmin.NewTokens(d.Cfg.PlatformJWTSecret, d.Cfg.PlatformAccessTTL), cipher, d.Redis, aud, d.Cfg.PlatformRefreshTTL)
		saSvc.SetFeaturesHook(dispatcher.FeaturesChanged)
	}

	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, middleware.Recoverer(d.Log), middleware.Logger(d.Log))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: d.Cfg.CORSOrigins,
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key"},
		MaxAge:         300,
	}))
	r.Get("/health", hl.Live)
	r.Get("/ready", hl.Ready)
	r.Get("/pay/{id}", payH.Checkout)
	r.Post("/pay/{id}/fake", payH.FakeConfirm)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/ping", hl.Live)
		r.Get("/ws", wsH.ServeHTTP)
		if saSvc != nil {
			superadmin.Mount(r, saSvc, d.Redis, d.Cfg.PlatformAllowedCIDRs)
		}

		r.Route("/auth", func(r chi.Router) {
			r.With(middleware.RateLimit(d.Redis, "otp", 20, time.Hour)).Post("/otp/request", authH.RequestOTP)
			r.With(middleware.RateLimit(d.Redis, "verify", 60, 10*time.Minute)).Post("/otp/verify", authH.VerifyOTP)
			r.With(middleware.RateLimit(d.Redis, "login", 60, 10*time.Minute)).Post("/login", authH.PasswordLogin)
			r.Post("/select-society", authH.SelectSociety)
			r.Post("/refresh", authH.Refresh)
			r.With(middleware.Auth(tokens)).Post("/change-password", authH.ChangePassword)
			r.With(middleware.Auth(tokens)).Post("/logout", authH.Logout)
		})

		r.Route("/me", func(r chi.Router) {
			// read-only identity calls stay available while a temporary password is pending
			r.Use(middleware.Auth(tokens), middleware.Tenant(societies))
			r.Get("/", meH.Me)
			r.Get("/features", meH.Features)

			r.Group(func(r chi.Router) {
				r.Use(middleware.RequirePasswordChanged)
				r.Put("/devices", meH.RegisterDevice)
				r.Get("/notifications", noteH.List)
				r.Get("/notifications/unread-count", noteH.UnreadCount)
				r.Post("/notifications/read-all", noteH.ReadAll)
				r.Post("/notifications/{id}/read", noteH.Read)
				r.Get("/notification-prefs", noteH.Prefs)
				r.Put("/notification-prefs", noteH.SetPref)
				r.Get("/contacts", contactsH.List)
				r.With(middleware.RequireFeature(feats, "deliveries"), need("visitor.approve")).Get("/delivery-rules", deliveryH.Rules)
				r.With(middleware.RequireFeature(feats, "deliveries"), need("visitor.approve")).Put("/delivery-rules", deliveryH.SetRule)
				r.With(middleware.RequireFeature(feats, "sos"), need("sos.trigger")).Get("/sos", sosH.Mine)
				r.Delete("/devices/{id}", meH.UnregisterDevice)
				r.With(need("profile.manage")).Put("/profile", householdH.UpdateProfile)
				r.With(need("profile.manage")).Get("/emergency-contact", householdH.GetEmergency)
				r.With(need("profile.manage")).Put("/emergency-contact", householdH.SetEmergency)
				r.With(need("unit.read")).Get("/unit", householdH.Unit)
				r.Route("/family", func(r chi.Router) {
					r.Use(need("family.manage"))
					r.Get("/", householdH.ListFamily)
					r.Post("/", householdH.AddFamily)
					r.Put("/{id}", householdH.UpdateFamily)
					r.Delete("/{id}", householdH.DeleteFamily)
				})
				r.Route("/vehicles", func(r chi.Router) {
					r.Use(need("vehicle.manage"))
					r.Get("/", householdH.ListVehicles)
					r.Post("/", householdH.AddVehicle)
					r.Put("/{id}", householdH.UpdateVehicle)
					r.Delete("/{id}", householdH.DeleteVehicle)
				})
			})
		})

		// Visitors (feature `visitors`): resident side, scoped to the caller's flat
		r.Route("/visitors", func(r chi.Router) {
			r.Use(protect("", "visitors")...)
			r.With(need("visitor.read")).Get("/", visitorH.List)
			r.With(need("visitor.read")).Get("/summary", visitorH.Summary)
			r.With(need("visitor.read")).Get("/{id}", visitorH.Get)
			r.With(middleware.RequireFeature(feats, "deliveries"), need("visitor.create")).Post("/expect-delivery", deliveryH.Expect)
			r.With(middleware.RequireFeature(feats, "deliveries"), need("visitor.create")).Delete("/expect-delivery/{id}", deliveryH.CancelExpectation)
			r.Get("/{id}/photo", visitorH.Photo) // residents of the flat, guards and admins; the service decides
			r.With(need("visitor.create")).Post("/", visitorH.Create)
			r.With(need("visitor.create")).Post("/{id}/cancel", visitorH.Cancel)
			r.With(need("visitor.approve")).Post("/{id}/decision", visitorH.Decide)
		})

		// Gate API for guards (the guard app arrives in M5)
		r.Route("/gate", func(r chi.Router) {
			r.Use(protect("gate.entry", "visitors")...)
			r.Get("/dashboard", gateH.Dashboard)
			r.Get("/inside", gateH.Inside)
			r.Get("/vehicles", gateH.Vehicles)
			r.Get("/passes/sync", gateH.PassesSync)
			r.Post("/events/batch", gateH.Batch)
			r.Post("/verify", visitorH.Verify)
			r.Get("/units", visitorH.GateUnits)
			r.Get("/visits", visitorH.GateList)
			r.Post("/visits", visitorH.WalkIn)
			r.Get("/visits/{id}", visitorH.GateGet)
			r.Post("/visits/{id}/photo", visitorH.UploadPhoto)
			r.Post("/visits/{id}/cancel", visitorH.GateCancel)
			r.Post("/visits/{id}/ring", visitorH.Ring)
			r.Post("/visits/{id}/check-in", visitorH.CheckIn)
			r.Post("/visits/{id}/check-out", visitorH.CheckOut)
		})

		// Notices (feature `community`), complaints, polls
		r.Route("/notices", func(r chi.Router) {
			r.Use(protect("notice.read", "community")...)
			r.Get("/", noticeH.List)
			r.Get("/unread-count", noticeH.UnreadCount)
			r.Get("/{id}", noticeH.Get)
			r.Get("/{id}/attachments/{aid}", noticeH.Attachment)
		})
		r.Route("/complaints", func(r chi.Router) {
			r.Use(protect("", "complaints")...)
			r.With(need("complaint.create")).Post("/", complaintH.Raise)
			r.With(need("complaint.read")).Get("/", complaintH.ListMine)
			r.With(need("complaint.read")).Get("/common", complaintH.ListCommon)
			r.With(need("complaint.read")).Get("/{id}", complaintH.GetMine())
			r.With(need("complaint.read")).Get("/{id}/attachments/{aid}", complaintH.Attachment)
			r.With(need("complaint.create")).Post("/{id}/photos", complaintH.Photo)
			r.With(need("complaint.create")).Post("/{id}/comments", complaintH.Comment)
			r.With(need("complaint.create")).Post("/{id}/reopen", complaintH.Reopen)
			r.With(need("complaint.create")).Post("/{id}/rate", complaintH.Rate)
		})
		r.Route("/amenities", func(r chi.Router) {
			r.Use(protect("amenity.book", "amenities")...)
			r.Get("/", amenityH.List)
			r.Get("/bookings", amenityH.Mine)
			r.Post("/bookings/{id}/cancel", amenityH.Cancel)
			r.Get("/{id}", amenityH.Get)
			r.Get("/{id}/slots", amenityH.Slots)
			r.Post("/{id}/bookings", amenityH.Book)
			r.Get("/{id}/photos/{aid}", amenityH.Photo)
		})
		r.Route("/helpers", func(r chi.Router) {
			r.Use(protect("helper.read", "daily_help")...)
			r.Get("/", helperH.List)
			r.Get("/{id}", helperH.Get())
			r.Get("/{id}/pass", helperH.Pass())
			r.Get("/{id}/photo", helperH.Photo())
			r.Group(func(r chi.Router) {
				r.Use(need("helper.manage"))
				r.Post("/", helperH.Register)
				r.Put("/{id}/schedule", helperH.UpdateLink)
				r.Delete("/{id}", helperH.Unlink())
				r.Post("/{id}/pass/regenerate", helperH.RegenerateCode())
				r.Post("/{id}/photo", helperH.UploadPhoto())
				r.Put("/{id}/rating", helperH.Rate)
			})
		})
		// Gate attendance for guards (daily help and society staff)
		r.Route("/attendance", func(r chi.Router) {
			r.Use(protect("attendance.log", "staff_attendance")...)
			r.Post("/resolve", attendH.Resolve)
			r.Post("/check-in", attendH.CheckIn())
			r.Post("/check-out", attendH.CheckOut())
			r.Get("/inside", attendH.Inside)
			r.Get("/photo/{type}/{id}", attendH.GatePhoto)
		})
		r.Route("/bills", func(r chi.Router) {
			r.Use(protect("bill.read", "payments")...)
			r.Get("/", billH.Bills)
			r.Get("/summary", billH.Summary)
			r.Get("/{id}", billH.Bill)
		})
		r.With(protect("bill.read", "payments")...).Get("/ledger", billH.Ledger)
		r.Route("/payments", func(r chi.Router) {
			r.Use(protect("", "payments")...)
			r.With(need("bill.read")).Get("/", billH.Payments)
			r.With(need("bill.read")).Get("/{id}/receipt", billH.Receipt())
			r.With(need("bill.read")).Get("/{id}/receipt.pdf", billH.ReceiptPDF())
			r.With(need("payment.pay")).Post("/orders", payH.CreateOrder)
			r.With(need("payment.pay")).Get("/orders/{id}", payH.Order)
		})
		r.Post("/webhooks/razorpay/{society}", payH.Razorpay)
		r.Route("/polls", func(r chi.Router) {
			r.Use(protect("poll.read", "polls")...)
			r.Get("/", pollH.List)
			r.Get("/{id}", pollH.Get())
			r.With(need("poll.vote")).Post("/{id}/vote", pollH.Vote)
		})

		// SOS (feature `sos`)
		r.Route("/sos", func(r chi.Router) {
			r.Use(protect("", "sos")...)
			r.With(need("sos.trigger")).Post("/", sosH.Raise)
			r.With(need("sos.trigger")).Post("/{id}/cancel", sosH.Cancel())
			r.With(need("sos.respond")).Get("/active", sosH.Active)
			r.With(need("sos.respond")).Post("/{id}/acknowledge", sosH.Acknowledge())
			r.With(need("sos.respond")).Post("/{id}/resolve", sosH.Resolve)
		})

		// Shift handling stays available when the Visitors service is off, so the app can show why.
		r.Route("/gate/shift", func(r chi.Router) {
			r.Use(protect("gate.shift", "")...)
			r.Get("/", gateH.Shift)
			r.Get("/gates", gateH.GuardGates)
			r.Post("/start", gateH.StartShift)
			r.Post("/end", gateH.EndShift)
		})

		r.Route("/society", func(r chi.Router) {
			r.Group(func(r chi.Router) { // notice board management
				r.Use(protect("notice.manage", "community")...)
				r.Get("/notices", noticeH.AdminList)
				r.Post("/notices", noticeH.Create)
				r.Get("/notices/{id}", noticeH.AdminGet)
				r.Put("/notices/{id}", noticeH.Update)
				r.Delete("/notices/{id}", noticeH.Archive)
				r.Post("/notices/{id}/publish", noticeH.Publish)
				r.Post("/notices/{id}/attachments", noticeH.Upload)
				r.Get("/notices/{id}/attachments/{aid}", noticeH.AdminFile)
				r.Delete("/notices/{id}/attachments/{aid}", noticeH.RemoveFile)
				r.Get("/notices/{id}/stats", noticeH.Stats)
			})
			r.Group(func(r chi.Router) { // complaints desk
				r.Use(protect("complaint.manage", "complaints")...)
				r.Get("/complaints", complaintH.AdminList)
				r.Get("/complaints/stats", complaintH.Stats)
				r.Get("/complaints/export", complaintH.Export)
				r.Get("/complaints/{id}", complaintH.AdminGet())
				r.Get("/complaints/{id}/attachments/{aid}", complaintH.AdminAttachment)
				r.Post("/complaints/{id}/assign", complaintH.Assign)
				r.Post("/complaints/{id}/status", complaintH.SetStatus)
				r.Post("/complaints/{id}/priority", complaintH.SetPriority)
				r.Post("/complaints/{id}/comments", complaintH.OfficeComment)
				r.Get("/office-members", complaintH.OfficeMembers)
			})
			r.Group(func(r chi.Router) { // amenities desk
				r.Use(protect("amenity.manage", "amenities")...)
				r.Get("/amenities", amenityH.AdminList)
				r.Post("/amenities", amenityH.Create)
				r.Get("/amenities/stats", amenityH.Stats)
				r.Get("/amenities/{id}", amenityH.AdminGet)
				r.Put("/amenities/{id}", amenityH.Update)
				r.Delete("/amenities/{id}", amenityH.Deactivate)
				r.Post("/amenities/{id}/blackouts", amenityH.AddBlackout)
				r.Delete("/amenities/{id}/blackouts/{bid}", amenityH.RemoveBlackout)
				r.Post("/amenities/{id}/photos", amenityH.AddPhoto)
				r.Get("/amenities/{id}/photos/{aid}", amenityH.Photo)
				r.Delete("/amenities/{id}/photos/{aid}", amenityH.RemovePhoto)
				r.Get("/bookings", amenityH.AdminBookings)
				r.Get("/bookings/export", amenityH.Export)
				r.Post("/bookings/{id}/approve", amenityH.Approve())
				r.Post("/bookings/{id}/reject", amenityH.Reject())
				r.Post("/bookings/{id}/cancel", amenityH.OfficeCancel())
				r.Post("/bookings/{id}/no-show", amenityH.NoShow())
			})
			r.Group(func(r chi.Router) { // daily help register (verification)
				r.Use(protect("helper.verify", "daily_help")...)
				r.Get("/helpers", helperH.AdminList)
				r.Get("/helpers/{id}", helperH.AdminGet())
				r.Get("/helpers/{id}/photo", helperH.AdminPhoto())
				r.Post("/helpers/{id}/photo", helperH.AdminUploadPhoto())
				r.Post("/helpers/{id}/verify", helperH.Decide("verify"))
				r.Post("/helpers/{id}/suspend", helperH.Decide("suspend"))
				r.Post("/helpers/{id}/blacklist", helperH.Decide("blacklist"))
				r.Post("/helpers/{id}/restore", helperH.Decide("restore"))
			})
			r.Group(func(r chi.Router) { // staff registry and manual attendance
				r.Use(protect("staff.manage", "staff_attendance")...)
				r.Get("/staff", attendH.ListStaff)
				r.Post("/staff", attendH.CreateStaff)
				r.Get("/staff/{id}", attendH.GetStaff)
				r.Put("/staff/{id}", attendH.UpdateStaff)
				r.Get("/staff/{id}/pass", attendH.StaffPass())
				r.Post("/staff/{id}/pass/regenerate", attendH.StaffNewCode())
				r.Post("/staff/{id}/photo", attendH.StaffPhoto)
				r.Post("/attendance/manual", attendH.Manual)
			})
			r.Group(func(r chi.Router) { // attendance sheets (admin + committee)
				r.Use(protect("attendance.read", "staff_attendance")...)
				r.Get("/attendance", attendH.Sheet)
				r.Get("/attendance/export", attendH.Export)
				r.Get("/attendance/{type}/{id}", attendH.Log)
			})
			r.Group(func(r chi.Router) { // billing: reads (admin + committee)
				r.Use(protect("report.read", "payments")...)
				r.Get("/billing/settings", billH.GetSettings)
				r.Get("/billing/heads", billH.Heads)
				r.Get("/invoices", billH.AdminInvoices)
				r.Get("/invoices/export", billH.ExportInvoices)
				r.Get("/invoices/{id}", billH.AdminInvoice)
				r.Get("/payments", billH.AdminPayments)
				r.Get("/payments/{id}/receipt", billH.OfficeReceipt())
				r.Get("/payments/{id}/receipt.pdf", billH.OfficeReceiptPDF())
				r.Get("/billing/dues", billH.Dues)
				r.Get("/billing/defaulters/export", billH.ExportDefaulters)
				r.Get("/billing/ledger/{id}", billH.FlatLedger)
				r.Get("/billing/reports/collection", billH.Collection)
			})
			r.Group(func(r chi.Router) { // billing: changes (admin)
				r.Use(protect("bill.manage", "payments")...)
				r.Put("/billing/settings", billH.PutSettings)
				r.Post("/billing/heads", billH.CreateHead)
				r.Put("/billing/heads/{id}", billH.UpdateHead)
				r.Delete("/billing/heads/{id}", billH.DeleteHead)
				r.Get("/billing/gateway", payH.GetGateway)
				r.Put("/billing/gateway", payH.PutGateway)
				r.Post("/billing/gateway/test", payH.TestGateway)
				r.Post("/invoices/generate", billH.Generate)
				r.Post("/invoices/issue", billH.Issue)
				r.Post("/invoices", billH.AdHoc)
				r.Post("/invoices/{id}/void", billH.Void)
				r.Post("/payments", billH.RecordPayment)
				r.Post("/payments/{id}/bounce", billH.Bounce)
				r.Post("/ledger/adjust", billH.Adjust)
				r.Post("/billing/remind", billH.Remind)
			})
			r.Group(func(r chi.Router) { // polls
				r.Use(protect("poll.manage", "polls")...)
				r.Get("/polls", pollH.AdminList)
				r.Post("/polls", pollH.Create)
				r.Get("/polls/{id}", pollH.AdminGet())
				r.Put("/polls/{id}", pollH.Update)
				r.Delete("/polls/{id}", pollH.Archive)
				r.Post("/polls/{id}/close", pollH.Close())
				r.Get("/polls/{id}/export", pollH.Export)
			})
			r.Group(func(r chi.Router) { // SOS log (the service limits it to the office roles)
				r.Use(protect("sos.respond", "sos")...)
				r.Get("/sos", sosH.AdminList)
				r.Get("/sos/export", sosH.AdminExport)
				r.Get("/sos/{id}", sosH.AdminGet())
			})
			r.Group(func(r chi.Router) { // emergency contacts
				r.Use(protect("society.manage", "")...)
				r.Get("/contacts", contactsH.List)
				r.Post("/contacts", contactsH.Create)
				r.Put("/contacts/{id}", contactsH.Update)
				r.Delete("/contacts/{id}", contactsH.Delete)
			})
			r.Group(func(r chi.Router) { // guards and gates
				r.Use(protect("guard.manage", "")...)
				r.Get("/guards", gateH.ListGuards)
				r.Post("/guards", gateH.CreateGuard)
				r.Patch("/guards/{id}", gateH.UpdateGuard)
				r.Delete("/guards/{id}", gateH.RemoveGuard)
				r.Get("/gates", gateH.ListGates)
				r.Post("/gates", gateH.CreateGate)
				r.Patch("/gates/{id}", gateH.UpdateGate)
				r.Get("/gate-settings", gateH.GetSettings)
				r.Patch("/gate-settings", gateH.UpdateSettings)
			})
			r.Group(func(r chi.Router) { // admin visitor log
				r.Use(protect("visitor.admin", "visitors")...)
				r.Get("/visitors", visitorH.AdminList)
				r.Get("/visitors/stats", visitorH.AdminStats)
				r.Get("/visitors/export", visitorH.AdminExport)
			})
			r.Group(func(r chi.Router) { // read access (admin + committee)
				r.Use(protect("resident.read", "")...)
				r.Get("/blocks", societyH.ListBlocks)
				r.Get("/units", societyH.ListUnits)
				r.Get("/units/{id}", societyH.GetUnit)
				r.Get("/apartments", societyH.Apartments)
				r.Get("/stats", societyH.Stats)
				r.Get("/residents", residentH.List)
				r.Get("/residents/{id}", residentH.Get)
			})
			r.Group(func(r chi.Router) { // society structure
				r.Use(protect("society.manage", "")...)
				r.Post("/blocks", societyH.CreateBlock)
				r.Put("/blocks/{id}", societyH.UpdateBlock)
				r.Delete("/blocks/{id}", societyH.DeleteBlock)
				r.Post("/blocks/{id}/units/bulk", societyH.BulkUnits)
				r.Post("/units", societyH.CreateUnit)
				r.Put("/units/{id}", societyH.UpdateUnit)
				r.Delete("/units/{id}", societyH.DeleteUnit)
			})
			r.Group(func(r chi.Router) { // resident management
				r.Use(protect("resident.manage", "")...)
				r.Post("/residents", residentH.Create)
				r.Put("/residents/{id}", residentH.Update)
				r.Post("/residents/{id}/suspend", residentH.Suspend())
				r.Post("/residents/{id}/restore", residentH.Restore())
				r.Post("/residents/{id}/move-out", residentH.MoveOut())
				r.Post("/residents/import", residentH.Import)
				r.Get("/residents/import/template", residentH.ImportTemplate)
			})
		})
	})
	return &App{Router: r, Protect: protect, Visitors: visitorSvc, Hub: hub, Sos: sosSvc, Notices: noticeSvc, Complaints: complaintSvc, Polls: pollSvc, Amenities: amenitySvc, Attendance: attendSvc, Billing: billSvc, Payments: paySvc, Notify: noteSvc, gates: gateSvc}
}
