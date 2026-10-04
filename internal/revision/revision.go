// Package revision stores a Service's config and history on its replicas'
// labels (gzip + base64 JSON), like Helm stores releases in Secrets. The
// history label is one gzip of every past entry's raw spec JSON, so
// near-identical consecutive specs compress against each other.
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
	"github.com/compose-spec/compose-go/v2/loader"
	composetransform "github.com/compose-spec/compose-go/v2/transform"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/docker/cli/pkg/kvfile"
)

const (
	LabelRevision = transform.LabelPrefix + "revision"
	LabelSpec     = transform.LabelPrefix + "spec"
	LabelSpecHash = transform.LabelPrefix + "spec-hash"
	LabelHistory  = transform.LabelPrefix + "history"
	LabelUpID     = transform.LabelPrefix + "up-id"
	LabelTime     = transform.LabelPrefix + "time"

	// HistoryBudget caps the encoded history label; the oldest entries
	// are dropped to fit.
	HistoryBudget = 64 << 10
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
	Revision int             `json:"revision"`
	Time     time.Time       `json:"time"`
	UpID     string          `json:"up_id"`
	Spec     json.RawMessage `json:"spec"` // the stored JSON, see Encode
}

// stored is what a spec label holds. EnvFile.Required does not survive
// compose-go's JSON (OptOut is omitzero and true counts as zero), so it is
// kept alongside; Decode reads absent as true instead, and Encode still
// writes it so existing spec hashes do not change.
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
	// The S-app entries transform adds next to each depends_on S only order
	// stop and down; they are not part of the spec.
	if app.DependsOn != nil {
		out.DependsOn = types.DependsOnConfig{}
		for k, v := range app.DependsOn {
			if base, ok := strings.CutSuffix(k, transform.AppName("")); ok {
				if _, added := app.DependsOn[base]; added {
					continue
				}
			}
			out.DependsOn[k] = v
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

// Encode returns the stripped config as JSON (what Decode reads) and its
// deterministic hash.
func Encode(app types.ServiceConfig) (spec json.RawMessage, hash string, err error) {
	s, err := Strip(app)
	if err != nil {
		return nil, "", err
	}
	st := stored{Service: s}
	for _, f := range s.EnvFiles {
		st.Required = append(st.Required, bool(f.Required))
	}
	j, err := json.Marshal(st)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(j)
	return j, hex.EncodeToString(sum[:8]), nil
}

// Decode reads a spec Encode wrote. compose-go writes some fields to JSON in
// a form its types cannot unmarshal (extra_hosts as a list, short ulimits,
// file modes as octal strings, OptOut fields omitted when true), so the JSON
// is read as a plain document and taken through compose-go's own loader
// steps (canonical syntax, defaults, decoding), the path a compose file
// takes. JSON stays the stored format, so existing labels and hashes match.
func Decode(spec json.RawMessage) (types.ServiceConfig, error) {
	var st struct {
		Service any `json:"service"`
	}
	d := json.NewDecoder(bytes.NewReader(spec))
	d.UseNumber()
	if err := d.Decode(&st); err != nil {
		return types.ServiceConfig{}, err
	}
	model := map[string]any{"services": map[string]any{"s": numbers(st.Service)}}
	model, err := composetransform.Canonical(model, false)
	if err == nil {
		model, err = composetransform.SetDefaultValues(model)
	}
	var app types.ServiceConfig
	if err == nil {
		err = loader.Transform(model["services"].(map[string]any)["s"], &app)
	}
	app.Name = ""
	return app, err
}

// numbers turns JSON numbers into the ints and floats a YAML document holds,
// which is what the loader's decoders expect.
func numbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
		f, _ := t.Float64()
		return f
	case []any:
		for i := range t {
			t[i] = numbers(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = numbers(t[k])
		}
	}
	return v
}

// Stamp returns app with all revision labels. current is the labels of a
// running replica of the current revision (nil if none). trimmedTo is the
// number of revisions kept (current included) when HistoryBudget dropped
// older ones, else 0.
func Stamp(app types.ServiceConfig, current map[string]string, upID string, historyMax int, now time.Time) (_ types.ServiceConfig, trimmedTo int, err error) {
	j, hash, err := Encode(app)
	if err != nil {
		return app, 0, err
	}
	spec, err := pack(j)
	if err != nil {
		return app, 0, err
	}
	rev := 1
	var history []Entry
	if current != nil && current[LabelRevision] != "" {
		prev, _ := strconv.Atoi(current[LabelRevision])
		rev = prev + 1
		all, err := History(current)
		if err != nil {
			return app, 0, err
		}
		history = all
	}
	if len(history) > historyMax {
		history = history[:max(historyMax, 0)]
	}
	h, err := pack(history)
	// ponytail: proportional cut, then one entry at a time; may drop a few
	// more than strictly needed when compression is very uneven.
	for n := len(history); err == nil && len(h) > HistoryBudget && n > 0; {
		n = min(n-1, n*HistoryBudget/len(h))
		h, err = pack(history[:n])
		trimmedTo = n + 1
	}
	if err != nil {
		return app, 0, err
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
	return out, trimmedTo, nil
}

// History returns the current revision first, then the stored history.
func History(labels map[string]string) ([]Entry, error) {
	rev, err := strconv.Atoi(labels[LabelRevision])
	if err != nil {
		return nil, fmt.Errorf("no revision label: %w", err)
	}
	t, _ := time.Parse(time.RFC3339, labels[LabelTime])
	out := []Entry{{Revision: rev, Time: t, UpID: labels[LabelUpID]}}
	if err := unpack(labels[LabelSpec], &out[0].Spec); err != nil {
		return nil, err
	}
	var past []Entry
	if labels[LabelHistory] != "" {
		if err := unpack(labels[LabelHistory], &past); err != nil {
			return nil, err
		}
	}
	return append(out, past...), nil
}
