// Module boundary for the web/ subtree. It carries no Go code of its own and
// nothing imports it; it exists solely so the Go toolchain does not walk into
// web/node_modules when it expands `./...` at the repo root. An npm dependency
// (flatted, transitively via eslint) ships a Go package, and without this
// boundary `go build ./...` / `go vet ./...` / `go test ./...` would pull that
// vendored package into the platform build graph. A nested module is excluded
// from the parent module's `./...`, which keeps the Go gate clean and
// deterministic whether or not node_modules is installed.
module github.com/maaxton/waiveo-next/web

go 1.26
