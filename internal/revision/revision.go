// Package revision stores a Service's config and history on its replicas'
// labels (gzip + base64 JSON), like Helm stores releases in Secrets.
//
// Every key defined in a service's env_file is stripped from the stored
// environment, and Restore reads the files again, so an undo picks up the
// current secrets. An explicit `environment:` key that also appears in an
// env_file is stripped too, so on undo it follows the file.
package revision

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/pkg/kvfile"
)

const (
	LabelRevision = "bouncer.revision"
	LabelSpec     = "bouncer.spec"
	LabelSpecHash = "bouncer.spec-hash"
	LabelHistory  = "bouncer.history"
	LabelUpID     = "bouncer.up-id"
	LabelTime     = "bouncer.time"
)

var revisionLabel = map[string]bool{LabelRevision: true, LabelSpec: true, LabelSpecHash: true,
	LabelHistory: true, LabelUpID: true, LabelTime: true}

// The `raw` env_file format is registered by the docker compose CLI, not by
// compose-go; register the same parser (docker run --env-file semantics).
func init() {
	dotenv.RegisterFormat("raw", func(r io.Reader, filename string, vars map[string]string, lookup func(string) (string, bool)) error {
		lines, err := kvfile.ParseFromReader(r, lookup)
		if err != nil {
			return fmt.Errorf("failed to parse env_file %s: %w", filename, err)
		}
		for _, line := range lines {
			k, v, _ := strings.Cut(line, "=")
			vars[k] = v
		}
		return nil
	})
}

type Entry struct {
	Revision int       `json:"revision"`
	Time     time.Time `json:"time"`
	UpID     string    `json:"up_id"`
	Spec     string    `json:"spec"`
}

// stored is what a spec label holds. EnvFile.Required does not survive
// compose-go's JSON (OptOut is omitzero and true counts as zero), so it is
// kept alongside.
type stored struct {
	Service  types.ServiceConfig `json:"service"`
	Required []bool              `json:"env_files_required,omitempty"`
}

// readEnvFiles parses the app's env_files like compose does: missing optional
// files are skipped, a missing required file is an error.
func readEnvFiles(app types.ServiceConfig, lookup dotenv.LookupFn) (map[string]string, error) {
	vars := map[string]string{}
	for _, f := range app.EnvFiles {
		fh, err := os.Open(f.Path)
		if os.IsNotExist(err) && !bool(f.Required) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("env file %s: %w", f.Path, err)
		}
		err = dotenv.ParseWithFormat(fh, f.Path, vars, lookup, f.Format)
		fh.Close()
		if err != nil {
			return nil, err
		}
	}
	return vars, nil
}

// Strip removes every environment key defined in an env_file (whatever its
// value), so secrets are never stored, and drops the revision labels.
func Strip(app types.ServiceConfig) (types.ServiceConfig, error) {
	// Only the keys matter: a lookup that always succeeds needs no
	// interpolation and never warns about unset variables. The placeholder
	// is non-empty so ${VAR:?msg} does not fail.
	keys, err := readEnvFiles(app, func(string) (string, bool) { return "x", true })
	if err != nil {
		return app, err
	}
	out := app
	out.Environment = types.MappingWithEquals{}
	for k, v := range app.Environment {
		if _, ok := keys[k]; !ok {
			out.Environment[k] = v
		}
	}
	// The replica count is not part of the spec: changing it alone scales
	// without a bounce. Copy Deploy; app's is shared.
	out.Scale = nil
	if app.Deploy != nil {
		d := *app.Deploy
		d.Replicas = nil
		out.Deploy = &d
		if reflect.ValueOf(d).IsZero() {
			out.Deploy = nil
		}
	}
	// Revision labels are bookkeeping, not part of the spec; every other
	// label is (a label change is a change).
	out.Labels = types.Labels{}
	for k, v := range app.Labels {
		if !revisionLabel[k] {
			out.Labels[k] = v
		}
	}
	return out, nil
}

// Restore reads the env_files now, resolving them against env (the project
// environment) and then the service environment, as compose does; explicit
// environment wins.
func Restore(app types.ServiceConfig, env map[string]string) (types.ServiceConfig, error) {
	fromFiles, err := readEnvFiles(app, func(k string) (string, bool) {
		if v, ok := env[k]; ok {
			return v, true
		}
		if v, ok := app.Environment[k]; ok && v != nil {
			return *v, true
		}
		return "", false
	})
	if err != nil {
		return app, err
	}
	out := app
	out.Environment = types.MappingWithEquals{}
	for k, v := range fromFiles {
		out.Environment[k] = &v
	}
	for k, v := range app.Environment {
		out.Environment[k] = v
	}
	return out, nil
}

func pack(v any) (string, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.ModTime = time.Time{} // deterministic output
	zw.Write(j)
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func unpack(s string, into any) error {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	j, err := io.ReadAll(zr)
	if err != nil {
		return err
	}
	return json.Unmarshal(j, into)
}

// Encode packs the stripped config; the hash is deterministic.
func Encode(app types.ServiceConfig) (spec, hash string, err error) {
	s, err := Strip(app)
	if err != nil {
		return "", "", err
	}
	st := stored{Service: s}
	for _, f := range s.EnvFiles {
		st.Required = append(st.Required, bool(f.Required))
	}
	j, err := json.Marshal(st)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(j)
	spec, err = pack(st)
	return spec, hex.EncodeToString(sum[:8]), err
}

func Decode(spec string) (types.ServiceConfig, error) {
	var st stored
	if err := unpack(spec, &st); err != nil {
		return types.ServiceConfig{}, err
	}
	for i := range st.Service.EnvFiles {
		st.Service.EnvFiles[i].Required = types.OptOut(i < len(st.Required) && st.Required[i])
	}
	return st.Service, nil
}

// Stamp returns app with all revision labels. current is the labels of a
// running replica of the current revision (nil if none).
func Stamp(app types.ServiceConfig, current map[string]string, upID string, historyMax int, now time.Time) (types.ServiceConfig, error) {
	spec, hash, err := Encode(app)
	if err != nil {
		return app, err
	}
	rev := 1
	var history []Entry
	if current != nil && current[LabelRevision] != "" {
		prev, _ := strconv.Atoi(current[LabelRevision])
		rev = prev + 1
		all, err := History(current)
		if err != nil {
			return app, err
		}
		history = all
	}
	if len(history) > historyMax {
		history = history[:max(historyMax, 0)]
	}
	h, err := pack(history)
	if err != nil {
		return app, err
	}
	out := app
	out.Labels = types.Labels{}
	for k, v := range app.Labels {
		out.Labels[k] = v
	}
	out.Labels[LabelRevision] = strconv.Itoa(rev)
	out.Labels[LabelSpec] = spec
	out.Labels[LabelSpecHash] = hash
	out.Labels[LabelHistory] = h
	out.Labels[LabelUpID] = upID
	out.Labels[LabelTime] = now.UTC().Format(time.RFC3339)
	return out, nil
}

// History returns the current revision first, then the stored history.
func History(labels map[string]string) ([]Entry, error) {
	rev, err := strconv.Atoi(labels[LabelRevision])
	if err != nil {
		return nil, fmt.Errorf("no revision label: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, labels[LabelTime])
	out := []Entry{{Revision: rev, Time: t, UpID: labels[LabelUpID], Spec: labels[LabelSpec]}}
	var past []Entry
	if labels[LabelHistory] != "" {
		if err := unpack(labels[LabelHistory], &past); err != nil {
			return nil, err
		}
	}
	return append(out, past...), nil
}
