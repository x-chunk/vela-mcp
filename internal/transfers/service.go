package transfers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
)

// Options supplies infrastructure explicitly. LookupIdentity must consult the
// durable OAuth grant on every capability use and reject revoked/expired grants.
// HTTP transfers never inherit the process's stdio credential or local roots.
type Options struct {
	BaseURL                          string
	DatabasePath                     string
	SpoolDirectory                   string
	Service                          *app.Service
	LookupIdentity                   func(context.Context, string) (access.Identity, error)
	TTL                              time.Duration
	MaxUploadBytes, MaxDownloadBytes int64
}

type Service struct {
	opts  Options
	repo  *repository
	spool string
	now   func() time.Time
	slots chan struct{}
}

// Open initializes durable capability state. Interrupted upload claims become
// uncertain and can be inspected rather than blindly replayed after a restart.
func Open(ctx context.Context, o Options) (*Service, error) {
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("transfer base URL must be an absolute HTTP(S) URL")
	}
	if o.DatabasePath == "" || o.Service == nil || o.Service.Resolver == nil || o.LookupIdentity == nil {
		return nil, errors.New("transfers require database, application service, and identity lookup")
	}
	if o.TTL == 0 {
		o.TTL = 10 * time.Minute
	}
	if o.TTL < time.Minute || o.TTL > time.Hour {
		return nil, errors.New("transfer TTL must be between one minute and one hour")
	}
	if o.MaxUploadBytes <= 0 {
		o.MaxUploadBytes = o.Service.Limits.MaxUploadBytes
	}
	if o.MaxDownloadBytes <= 0 {
		o.MaxDownloadBytes = o.Service.Limits.MaxDownloadBytes
	}
	if o.MaxUploadBytes <= 0 || o.MaxDownloadBytes <= 0 || o.MaxUploadBytes > 1<<40 || o.MaxDownloadBytes > 1<<40 {
		return nil, errors.New("transfer byte limits must be positive and at most one TiB")
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	r, err := openRepository(ctx, o.DatabasePath)
	if err != nil {
		return nil, err
	}
	spool := o.SpoolDirectory
	if spool == "" {
		spool = filepath.Join(filepath.Dir(o.DatabasePath), "transfer-spool")
	}
	if err := os.MkdirAll(spool, 0700); err != nil {
		r.db.Close()
		return nil, err
	}
	if err := os.Chmod(spool, 0700); err != nil {
		r.db.Close()
		return nil, err
	}
	// Private transient files are intentionally not persisted across restart.
	entries, err := os.ReadDir(spool)
	if err != nil {
		r.db.Close()
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "transfer-") && !entry.IsDir() {
			if err := os.Remove(filepath.Join(spool, entry.Name())); err != nil {
				r.db.Close()
				return nil, err
			}
		}
	}
	return &Service{opts: o, repo: r, spool: spool, now: time.Now, slots: make(chan struct{}, 4)}, nil
}

func (s *Service) Close() error { return s.repo.db.Close() }

type UploadRequest struct {
	Path     string       `json:"path"`
	Size     int64        `json:"size"`
	SHA256   string       `json:"sha256"`
	Metadata app.Metadata `json:"metadata,omitempty"`
}

// UploadGrant gives clients raw-body HTTP instructions. The capability may be
// sent to the advertised endpoint only; do not forward it to Vela or other hosts.
type UploadGrant struct {
	TransferID   string            `json:"transfer_id"`
	UploadURL    string            `json:"upload_url"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Size         int64             `json:"size"`
	SHA256       string            `json:"sha256"`
	MaxBytes     int64             `json:"max_bytes"`
	Instructions string            `json:"instructions"`
}

type DownloadGrant struct {
	TransferID   string      `json:"transfer_id"`
	DownloadURL  string      `json:"download_url"`
	Method       string      `json:"method"`
	ExpiresAt    time.Time   `json:"expires_at"`
	Filename     string      `json:"filename"`
	Object       *api.Object `json:"object"`
	MaxUses      int         `json:"max_uses"`
	Instructions string      `json:"instructions"`
}

// Status reveals transfer receipts only to the authenticated owner. It never
// returns a capability URL; clients retain preparation results separately.
type Status struct {
	TransferID string            `json:"transfer_id"`
	Direction  string            `json:"direction"`
	Path       string            `json:"path"`
	State      string            `json:"state"`
	ExpiresAt  time.Time         `json:"expires_at"`
	Uses       int               `json:"uses"`
	Upload     *app.UploadResult `json:"upload,omitempty"`
	Object     *api.Object       `json:"object,omitempty"`
	Recovery   string            `json:"recovery,omitempty"`
}

func randomValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func capabilityHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func (s *Service) create(ctx context.Context, subject, kind string, binding any) (record, string, error) {
	id, err := randomValue()
	if err != nil {
		return record{}, "", err
	}
	token, err := randomValue()
	if err != nil {
		return record{}, "", err
	}
	b, err := json.Marshal(binding)
	if err != nil {
		return record{}, "", err
	}
	now := s.now().UTC()
	v := record{ID: id, Subject: subject, Kind: kind, State: "prepared", Hash: capabilityHash(token), Binding: b, ExpiresAt: now.Add(s.opts.TTL).Unix(), UpdatedAt: now.Unix()}
	if err := s.repo.insert(ctx, v, now.Unix()); err != nil {
		if errors.Is(err, errPendingQuota) {
			return record{}, "", &app.Invalid{Message: "too many pending file transfers; complete existing transfers or wait for their expiry"}
		}
		return record{}, "", err
	}
	return v, token, nil
}

func (s *Service) PrepareUpload(ctx context.Context, request UploadRequest) (*UploadGrant, error) {
	w, err := s.opts.Service.Open(ctx)
	if err != nil {
		return nil, err
	}
	if request.Size < 0 || request.Size > s.opts.MaxUploadBytes {
		return nil, &app.Invalid{Message: "size exceeds the HTTP upload limit"}
	}
	b, err := w.PrepareStreamUpload(request.Path, request.Size, request.SHA256, request.Metadata)
	if err != nil {
		return nil, err
	}
	v, token, err := s.create(ctx, w.Identity().Subject, "upload", b)
	if err != nil {
		return nil, err
	}
	return &UploadGrant{TransferID: v.ID, UploadURL: s.opts.BaseURL + "/transfers/uploads/" + v.ID, Method: "PUT", Headers: map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/octet-stream"}, ExpiresAt: time.Unix(v.ExpiresAt, 0).UTC(), Size: b.Size, SHA256: b.SHA256, MaxBytes: s.opts.MaxUploadBytes, Instructions: "Send the original file bytes as the PUT request body from your computing environment. Do not encode them as base64. The upload is single-use and create-only. Check get_file_transfer_status after a lost response; prepare a new grant after a failed body validation."}, nil
}

func (s *Service) PrepareDownload(ctx context.Context, p string) (*DownloadGrant, error) {
	w, err := s.opts.Service.Open(ctx)
	if err != nil {
		return nil, err
	}
	obj, err := w.Stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if obj.Size < 0 || obj.Size > s.opts.MaxDownloadBytes || obj.Size > s.opts.Service.Limits.MaxDownloadBytes {
		return nil, &app.Invalid{Message: "file exceeds the HTTP download limit"}
	}
	v, token, err := s.create(ctx, w.Identity().Subject, "download", obj)
	if err != nil {
		return nil, err
	}
	return &DownloadGrant{TransferID: v.ID, DownloadURL: s.opts.BaseURL + "/transfers/downloads/" + v.ID + "?token=" + url.QueryEscape(token), Method: "GET", ExpiresAt: time.Unix(v.ExpiresAt, 0).UTC(), Filename: path.Base(obj.Path), Object: obj, MaxUses: 32, Instructions: "Download the original bytes into your computing environment and verify the object SHA-256. The URL also works in a browser and supports Range requests. Attach the downloaded file using your host's file-output mechanism, or share this expiring URL. The URL is a secret; it does not change file visibility. No Vela or OAuth token is needed."}, nil
}

func (s *Service) Status(ctx context.Context, id string) (*Status, error) {
	w, err := s.opts.Service.Open(ctx)
	if err != nil {
		return nil, err
	}
	v, err := s.repo.get(ctx, id)
	if err != nil || v.Subject != w.Identity().Subject {
		return nil, access.ErrForbidden
	}
	out := &Status{TransferID: v.ID, Direction: v.Kind, State: v.State, ExpiresAt: time.Unix(v.ExpiresAt, 0).UTC(), Uses: v.Uses}
	if v.Kind == "upload" {
		var b app.StreamUpload
		if err := json.Unmarshal(v.Binding, &b); err != nil {
			return nil, err
		}
		out.Path = b.Path
		if v.State == "succeeded" {
			if err := json.Unmarshal(v.Receipt, &out.Upload); err != nil {
				return nil, err
			}
		} else if v.State == "uncertain" {
			// An interrupted request may have committed in Vela. Observing the
			// bound content assists recovery but is not proof that this request
			// created it, so uncertainty is deliberately retained.
			if obj, err := w.Stat(ctx, b.Path); err == nil && obj.Size == b.Size && strings.EqualFold(obj.SHA256, b.SHA256) {
				out.Object = obj
				out.Recovery = "Matching content exists at the destination. Inspect its metadata before deciding the outcome; no overwrite or retry was performed."
			} else {
				out.Recovery = "The upstream result is unknown. Inspect the destination before preparing another create-only upload."
			}
		}
	} else {
		if err := json.Unmarshal(v.Binding, &out.Object); err != nil {
			return nil, err
		}
		out.Path = out.Object.Path
	}
	if v.State == "prepared" && s.now().Unix() >= v.ExpiresAt {
		out.State = "expired"
	}
	return out, nil
}

func (s *Service) workspace(ctx context.Context, subject string) (*app.Workspace, error) {
	i, err := s.opts.LookupIdentity(ctx, subject)
	if err != nil || i.Subject != subject {
		return nil, access.ErrForbidden
	}
	// Consumption has its own identity boundary even if a host accidentally
	// supplies a stdio Static resolver. Never inherit its credential or roots.
	service := *s.opts.Service
	service.Resolver = access.ContextResolver{}
	service.Files = nil
	return service.Open(access.WithIdentity(ctx, i))
}
