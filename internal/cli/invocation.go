package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/types"
)

// invocation is how an up or undo loaded its project, stored on every
// replica it creates (transform.LabelInvocation) so refresh can load the
// project the same way from any directory. Env files are paths only, never
// their contents. Command is "up" or "undo"; Revision is the one an undo
// restored, which holds the Service there until the next up.
type invocation struct {
	Version  string   `json:"version"`
	Command  string   `json:"command"`
	Revision int      `json:"revision,omitempty"`
	Files    []string `json:"files"`
	Dir      string   `json:"dir"`
	Name     string   `json:"name"`
	EnvFiles []string `json:"env_files,omitempty"`
	Profiles []string `json:"profiles,omitempty"`
}

// newInvocation records p as loaded with pf: the files, directory and
// profiles compose resolved, and the env files made absolute (compose reads
// relative ones from the current directory).
func newInvocation(p *types.Project, pf *ProjectFlags) invocation {
	inv := invocation{Version: Version, Command: "up", Files: p.ComposeFiles, Dir: p.WorkingDir, Name: p.Name}
	for _, f := range pf.EnvFiles {
		if abs, err := filepath.Abs(f); err == nil {
			f = abs
		}
		inv.EnvFiles = append(inv.EnvFiles, f)
	}
	for _, pr := range p.Profiles {
		if pr != "" { // an unset COMPOSE_PROFILES splits to ""
			inv.Profiles = append(inv.Profiles, pr)
		}
	}
	return inv
}

// flags are the ProjectFlags that load the project as inv's command did.
func (inv invocation) flags(progress string, timestamps bool) *ProjectFlags {
	return &ProjectFlags{Files: inv.Files, Name: inv.Name, Dir: inv.Dir, EnvFiles: inv.EnvFiles,
		Profiles: inv.Profiles, Progress: progress, Timestamps: timestamps}
}

// encode is base64(gzip(JSON)), like the revision labels.
func (inv invocation) encode() string {
	j, _ := json.Marshal(inv) // strings and ints only: cannot fail
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(j)
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// decodeInvocation reads a label written by encode; ok is false when there
// is none (a replica made before invocations were recorded).
func decodeInvocation(s string) (inv invocation, ok bool, err error) {
	if s == "" {
		return inv, false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return inv, false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return inv, false, err
	}
	j, err := io.ReadAll(zr)
	if err != nil {
		return inv, false, err
	}
	return inv, true, json.Unmarshal(j, &inv)
}
