# OpenAPI tooling

There is **no** OpenAPI or Swagger code in the framework: it does not generate specs, does not host a docs page, and does not know what a schema is. This page is about **wiring the existing packages in** — the default, comment-driven route comes with a tested recipe and four traps; the other two routes get just enough to judge them; and the last section covers what a mounted router costs you.

## Why the framework doesn't do it

Three hard facts hold at once, and that is what makes it a "don't":

- the handler is `func(*web.Ctx) error`, which carries **no type information** — the schema a spec needs cannot be derived from the signature;
- routes go straight into the standard library `ServeMux`, so the engine **keeps no route table** — there is no registry to enumerate;
- the stdlib `ServeMux` itself has **no enumeration API**: `Handle`, `HandleFunc`, `Handler(r)`, `ServeHTTP`, and that is all.

Generating a spec inside the framework would mean inventing a whole declaration vocabulary (schema / parameters / responses / security). That vocabulary would be a strictly worse copy of OpenAPI, and it would break three standing rules at once: zero third-party dependencies in the core module, no domain semantics in the framework, no escape hatches.

The ecosystem agrees: gin, chi, fiber, beego and go-zero mention OpenAPI nowhere in their READMEs; echo and hertz only list an entry under "third-party middleware". **Only frameworks built around OpenAPI** (huma, Fuego, goa) ship it — and they changed the shape of a handler to do so.

## The default route: comment-driven (swaggo)

The handler stays as it is; you add annotations above it:

```go
// @Summary      Get one user
// @Tags         users
// @Param        id   path      int  true  "User ID"
// @Success      200  {object}  main.User
// @Router       /users/{id} [get]
func getUser(c *web.Ctx) error {
	return c.JSON(http.StatusOK, User{ID: 1, Name: c.Path("id")})
}
```

Dependencies come in two halves: the UI is `github.com/swaggo/http-swagger/v2` (an ordinary dependency); the generator `swag` is a **CLI** and never enters `go.mod`:

```bash
go install github.com/swaggo/swag/cmd/swag@v1.16.4
swag init -g main.go -o docs      # writes docs/docs.go + swagger.json + swagger.yaml
```

Mounting it is plain `http.Handler` work:

```go
import (
	web "github.com/Luo-root/pulse-web"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	_ "yourapp/docs" // swag init output: its init() registers the spec with swaggo
)

app := web.New()
app.GET("/users/{id}", getUser)
app.Handle("/swagger/", web.Wrap(httpSwagger.WrapHandler))
```

`/swagger/index.html` is the UI, `/swagger/doc.json` is the spec itself; both travel the framework's middleware chain and observability — in the access log they carry their own trace id.

### Four traps

1. **`swag init` is a build step.** The generated `docs/` must either be committed or produced in CI first: the code imports `_ "yourapp/docs"`, so a missing directory fails the build.
2. **The CLI and the library must match versions.** Pin `github.com/swaggo/swag` in `go.mod` to the CLI's version — the `docs.go` written by CLI v1.16.4 uses `LeftDelim` / `RightDelim`, while `go mod tidy` may resolve a much older `v1.8.1` that lacks both fields, which only fails at compile time with `unknown field`.
3. **Response bodies must be named types.** `c.JSON(200, map[string]any{...})` yields no schema: define a struct for the response, tag fields with `json` and `example`, and annotate `@Success 200 {object} main.User`.
4. **The stable line stops at Swagger 2.0.** The first line of the generated spec is `"swagger": "2.0"`; for 3.x see the next section.

### Wanting OpenAPI 3.0: convert it

`kin-openapi` ships a 2.0 → 3.0 converter. It emits `openapi: 3.0.3` with every path and schema preserved, and passes the official validation:

```go
var doc2 openapi2.T
json.Unmarshal(raw, &doc2)              // raw = the swagger.json swag produced
doc3, err := openapi2conv.ToV3(&doc2)
if err != nil {
	return err
}
if err := doc3.Validate(context.Background()); err != nil {
	return err
}
```

Wire it into CI (convert once after the build and publish the 3.0 spec). **Do not hand-edit the converted file**: the next `swag init` will overwrite its source.

## The other two routes

**Spec first (`oapi-codegen` / `ogen`)**: write `openapi.yaml` first, then generate types and a server skeleton from it. `oapi-codegen` has a `std-http-server` template aimed at the standard library `net/http` (Go 1.22+), and ogen's generated `Server` is itself an `http.Handler` — both mount through `web.Wrap`. It fits teams where the contract precedes the implementation; the price is writing YAML first, and generated code that does not read like Go.

**Code first, ride along (huma)**: huma treats OpenAPI as a first-class citizen (it emits 3.1 and serves `/openapi.json` plus `/docs`), and it **ships an official adapter for the standard library `ServeMux`**:

```go
sub := http.NewServeMux()
api := humago.New(sub, huma.DefaultConfig("My API", "1.0.0"))
app.Handle("/api/", web.Wrap(sub))
```

Its handler shape is `func(ctx, *In) (*Out, error)`, not `func(*web.Ctx) error` — inside that subtree there is no `c` to use.

## What mounting costs you

Both routes above (huma included) mount **an entire routing tree** as a single `http.Handler`. The framework's `ServeMux` only sees a subtree pattern, so:

- the `http.route` on access logs and spans becomes the mount prefix (`/api/`) instead of **per-route**: trace id, duration and status stay, but that column gets coarse;
- middleware and parameter parsing inside the subtree belong to that library; the framework's middleware only applies on the outside.

The comment-driven route is unaffected: the handler is still a framework handler, and route attribution keeps the `http.route` contract described in [observability](/en/guide/observability).

## Verify it yourself

```bash
go install github.com/swaggo/swag/cmd/swag@v1.16.4
swag init -g main.go -o docs
go run . && curl -s -o /dev/null -w '%{http_code} %{http_code}\n' localhost:8080/swagger/index.html localhost:8080/swagger/doc.json
```

Both should print `200`; then check the startup log for your own routes — the `route=` column must not have turned into `/swagger/`.
