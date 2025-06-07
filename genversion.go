//go:build ignore

// This is not part of the package, it is a helper to generate the version.go
// file using `go generate`. It is not included in the build.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
)

const version_go = `package {{.Package}}

const Version = "{{.Version}}"
const Commit = "{{.Commit}}"
`

func cmdOutput(cmd string, args ...string) (string, error) {
	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func main() {
	outTemplate := template.Must(template.New("version_go").Parse(version_go))

	version, err := cmdOutput("git", "describe", "--tags", "--always", "--dirty")
	if err != nil {
		version = fmt.Sprintf("unknown (error %s)", err)
	}

	commit, err := cmdOutput("git", "rev-parse", "HEAD")
	if err != nil {
		commit = fmt.Sprintf("unknown (error %s)", err)
	}

	pkgName := "main"
	if len(os.Args) > 1 {
		pkgName = os.Args[1]
	}

	f, err := os.Create("version.go")
	if err != nil {
		panic(err)
	}
	err = outTemplate.Execute(f, struct {
		Package string
		Version string
		Commit  string
	}{pkgName, version, commit})
	if err != nil {
		panic(err)
	}
}
