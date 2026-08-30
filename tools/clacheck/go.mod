// Deliberately its own module: the CLA gate is CI tooling, not part of Pug. It
// stays out of `go build ./...`, out of `make lint`, and out of the release
// images, and it adds nothing to the application's dependency graph. Stdlib only,
// so a run needs no module download.
module github.com/pug-sh/pug/tools/clacheck

go 1.26.6
