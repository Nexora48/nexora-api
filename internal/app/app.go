// Package app wires every module into one chi router (used by main and tests).
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/blob"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/gate"
	"github.com/Nexora48/nexora-api/internal/health"
	"github.com/Nexora48/nexora-api/internal/household"
	"github.com/Nexora48/nexora-api/internal/live"
	"github.com/Nexora48/nexora-api/internal/middleware"
	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/rbac"
	"github.com/Nexora48/nexora-api/internal/realtime"
	"github.com/Nexora48/nexora-api/internal/resident"
	"github.com/Nexora48/nexora-api/internal/society"
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
	Visitors *visitor.Service
	Hub      *realtime.Hub
	gates    *gate.Service
}

// Start runs the background jobs (visit expiry, shift cleanup, cross-instance event bridge) until ctx ends.
func (a *App) Start(ctx context.Context) {
	go a.Visitors.RunSweeper(ctx, time.Minute)
	go a.gates.RunSweeper(ctx, 10*time.Minute)
	go a.Hub.Run(ctx)
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
	dispatcher := live.NewDispatcher(hub, notify.NewService(d.Pool, sender, d.Log), d.Pool, d.Cfg.VisitorWalkinWait)
	visitorSvc.SetEvents(dispatcher)
	if d.Cfg.BlobPath != "" {
		if store, err := blob.NewDisk(d.Cfg.BlobPath); err == nil {
			visitorSvc.SetBlobs(store)
		} else {
			d.Log.Error("photo storage disabled", "err", err)
		}
	}
	gateSvc := gate.NewService(d.Pool, aud, hub)
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

		// Shift handling stays available when the Visitors service is off, so the app can show why.
		r.Route("/gate/shift", func(r chi.Router) {
			r.Use(protect("gate.shift", "")...)
			r.Get("/", gateH.Shift)
			r.Get("/gates", gateH.GuardGates)
			r.Post("/start", gateH.StartShift)
			r.Post("/end", gateH.EndShift)
		})

		r.Route("/society", func(r chi.Router) {
			r.Group(func(r chi.Router) { // guards and gates
				r.Use(protect("guard.manage", "")...)
				r.Get("/guards", gateH.ListGuards)
				r.Post("/guards", gateH.CreateGuard)
				r.Patch("/guards/{id}", gateH.UpdateGuard)
				r.Delete("/guards/{id}", gateH.RemoveGuard)
				r.Get("/gates", gateH.ListGates)
				r.Post("/gates", gateH.CreateGate)
				r.Patch("/gates/{id}", gateH.UpdateGate)
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
	return &App{Router: r, Protect: protect, Visitors: visitorSvc, Hub: hub, gates: gateSvc}
}
