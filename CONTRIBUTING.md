# Contributing

1. Branch from `main`.
2. Put new code in the area it belongs to (see docs/ARCHITECTURE.md); prefer the generic resource engine for plain master data.
3. Run `make check` (gofmt, vet, tests). New behaviour needs a test in `internal/app/app_test.go`; anything touching permissions, money or
   security needs a test for the failure case too.
4. Open a pull request. CI must be green.

Commit messages: one short imperative line, then a short explanation if the reason is not obvious.
