// Package revision stores a Service's config and history on its replicas'
// labels (gzip + base64 JSON), like Helm stores releases in Secrets.
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
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/types"
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

var osStat = os.Stat

type Entry struct {
	Revision int       `json:"revision"`
	Time     time.Time `json:"time"`
	UpID     string    `json:"up_id"`
	Spec     string    `json:"spec"`
}

// envFiles reads the app's env_file values now; missing optional files are skipped.
func envFiles(app types.ServiceConfig) (map[string]string, error) {
	var paths []string
	for _, f := range app.EnvFiles {
		if _, err := osStat(f.Path); err == nil || bool(f.Required) {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	return dotenv.GetEnvFromFile(map[string]string{}, paths)
}

// Strip removes environment variables whose value came from env_file, so
// secrets are never stored, and drops the revision labels themselves.
func Strip(app types.ServiceConfig) (types.ServiceConfig, error) {
	fromFiles, err := envFiles(app)
	if err != nil {
		return app, err
	}
	out := app
	out.Environment = types.MappingWithEquals{}
	for k, v := range app.Environment {
		if fv, ok := fromFiles[k]; ok && v != nil && *v == fv {
			continue
		}
		out.Environment[k] = v
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

// Restore re-reads EnvFiles now; explicit environment wins.
func Restore(app types.ServiceConfig) (types.ServiceConfig, error) {
	fromFiles, err := envFiles(app)
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
	j, err := json.Marshal(s)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(j)
	spec, err = pack(s)
	return spec, hex.EncodeToString(sum[:8]), err
}

func Decode(spec string) (types.ServiceConfig, error) {
	var s types.ServiceConfig
	err := unpack(spec, &s)
	return s, err
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
		history = history[:historyMax]
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
