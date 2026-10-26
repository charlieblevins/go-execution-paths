# test to see what showing execution paths looks like

A local web app for reading Go code in execution order. Pick a function, choose
one of its callers, and the caller's code (ending at the call line) is shown
directly above the callee. Keep choosing callers to build a top-down call stack.

## Usage

```sh
go install github.com/charlieblevins/go-execution-paths@latest   # or: go build -o gep .
go-execution-paths /path/to/module          # then open http://localhost:8080
go-execution-paths /path/to/module -tests   # include _test.go files
go-execution-paths -dir /path/to/module ./cmd/... ./internal/...   # custom package patterns
```

Pick a file in the tree, click a function definition to list its callers, and
pick one to show it above. Use the ↻ button after editing code to reload
packages from disk.

Build with a Go toolchain at least as new as the module you're analyzing
(e.g. `GOTOOLCHAIN=go1.27.0 go install ...`), since type-checking uses the
toolchain the binary was built with.

## How it works

At startup the module is loaded with `golang.org/x/tools/go/packages`. Every
call expression is resolved through `go/types` to the function it calls, which
builds a callee → call-sites index. Snippets are highlighted with chroma: each
file is lexed whole, then sliced by line.

Only static, direct calls are found. Calls through interfaces or function
values aren't followed yet (a possible next step is `go/callgraph/vta`).
