# github/nallanos/fire2 (modular monolith)

## Run

- `make run`
- `PORT=8081 make run`

## API

- `GET /health` -> `ok`
- `POST /api/builds` -> create build
  - body: `{ "repo": "github.com/acme/myrepo", "ref": "main" }`
- `GET /api/builds` -> list builds
- `GET /api/builds/{id}` -> get build
