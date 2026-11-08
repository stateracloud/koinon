module github.com/stateracloud/koinon/tools/mutation

go 1.27

tool github.com/avito-tech/go-mutesting/cmd/go-mutesting

require (
	github.com/avast/retry-go v3.0.0+incompatible // indirect
	github.com/avito-tech/go-mutesting v0.0.0-20261002081621-9ce5f731e321 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/jessevdk/go-flags v1.5.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/schollz/progressbar/v3 v3.14.2 // indirect
	github.com/stretchr/testify v1.9.0 // indirect
	github.com/termie/go-shutil v0.0.0-20140729215957-bcacb06fecae // indirect
	github.com/ulikunitz/xz v0.5.11 // indirect
	github.com/zimmski/osutil v1.6.1 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/term v0.17.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Our own patch (statement/remove walks select arms, i.e. *ast.CommClause) on top of the upstream
// commit 9ce5f731e321 (2026-10-02, 37 days old at the time of the spike). The patch commit itself is
// younger than ADR 0079's 3 days: a first-party exception, because the code is ours and reviewed here.
// Exit condition: once the patch is merged upstream and 3 days old, delete this replace and require
// the upstream version (issue 401).
replace github.com/avito-tech/go-mutesting => github.com/stateracloud/go-mutesting v0.0.0-20261108180448-a04e65a00fdc
