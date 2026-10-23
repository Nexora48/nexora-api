# nexora-api
Go backend (modular monolith) for the Nexora residential platform. See `../DEVELOPMENT_PLAN.md` and `../SKILL.md`.

```bash
make up && make migrate-up   # postgres, redis, minio (needs Docker, golang-migrate)
make run                     # http://localhost:8080/health
make test
```
