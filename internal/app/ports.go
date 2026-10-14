// Package app implements Vela workflows. It depends on ports, access policy and
// Vela's public value types, never on MCP, HTTP clients, config, or a database.
package app

import (
	"context"
	"errors"
	"io"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

// Gateway is the REST boundary. api types are upstream public value contracts;
// implementation details (headers, retries, bearer tokens) stay in the adapter.
type Gateway interface {
	Info(context.Context) (*api.Info, error)
	List(context.Context, ListOptions) (*api.Listing, error)
	Stat(context.Context, string) (*api.Object, error)
	Upload(context.Context, string, io.Reader, UploadOptions) (*api.Object, bool, error)
	Download(context.Context, string) (io.ReadCloser, error)
	Patch(context.Context, string, api.ObjectPatch, int64) (*api.Object, error)
	Move(context.Context, api.MoveRequest) (*api.MoveResponse, error)
	Delete(context.Context, string, DeleteOptions) (*api.DeleteResponse, error)
	ListTrash(context.Context, TrashOptions) (*api.TrashList, error)
	Restore(context.Context, string, api.RestoreRequest) (*api.Object, error)
	PurgeTrashEntry(context.Context, string) error
	PurgeTrash(context.Context, string) (*api.PurgeResponse, error)
	CreateLink(context.Context, api.CreateLinkRequest) (*api.Link, error)
	ListLinks(context.Context, LinkOptions) (*api.LinkList, error)
	RevokeLink(context.Context, string) error
	ListDirPolicies(context.Context, DirListOptions) (*api.DirPolicyList, error)
	ShowDir(context.Context, string) (*api.EffectiveDir, error)
	PatchDir(context.Context, string, api.DirPolicyPatch, int64) (*api.EffectiveDir, error)
	DeleteDir(context.Context, string, int64) error
}

type GatewayFactory interface {
	Open(context.Context, access.Credential) (Gateway, error)
}

// ErrUnavailable is a transport-neutral failure to reach the upstream store.
var ErrUnavailable = errors.New("upstream store is unavailable")

type ListOptions struct {
	Prefix     string
	Recursive  bool
	Visibility []api.Visibility
	Limit      int
	Cursor     string
}
type UploadOptions struct {
	Size               int64
	ContentType        string
	Visibility         api.Visibility
	Title, Description *string
	Tags               []string
	Disposition        api.Disposition
	ExpiresAt          *time.Time
	ClearExpiry        bool
	SHA256             string
	CreateOnly         bool
	IfRevision         int64
}
type DeleteOptions struct {
	Recursive, Purge bool
	IfRevision       int64
}
type TrashOptions struct {
	Prefix, Path string
	Limit        int
	Cursor       string
}
type LinkOptions struct {
	Path                           string
	IncludeExpired, IncludeRevoked bool
	Limit, Offset                  int
}
type DirListOptions struct {
	Prefix              string
	Recursive, Orphaned bool
	Limit               int
	Cursor              string
}

// LocalFiles is optional filesystem capability, explicitly supplied by the host.
// It is not inferred from a user-provided path or an MCP client's roots.
type LocalFiles interface {
	OpenUpload(string, int64) (ReadSeekCloser, int64, error)
	SaveDownload(context.Context, string, io.Reader, int64, string) (int64, error)
}
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

type Limits struct{ MaxUploadBytes, MaxInlineBytes, MaxReadBytes, MaxDownloadBytes int64 }
