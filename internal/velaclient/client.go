// Package velaclient adapts the public Vela Go client to application ports.
// A client is built from the resolved caller credential, never a global admin key.
package velaclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
	"go.xchunk.org/vela/client"
)

type Factory struct {
	BaseURL string
	Timeout time.Duration
	Retries int
}

func (f Factory) Open(_ context.Context, credential access.Credential) (app.Gateway, error) {
	c, err := client.New(f.BaseURL, credential.Token(), client.WithHTTPClient(&http.Client{Timeout: f.Timeout}), client.WithRetries(f.Retries), client.WithUserAgent("vela-mcp/0.1.0"))
	if err != nil {
		return nil, err
	}
	return &gateway{c}, nil
}

type gateway struct{ c *client.Client }

func classify(err error) error {
	var network *client.NetworkError
	if errors.As(err, &network) {
		return fmt.Errorf("%w: %w", app.ErrUnavailable, err)
	}
	return err
}
func result[T any](value T, err error) (T, error) { return value, classify(err) }

var _ app.Gateway = (*gateway)(nil)

func (g *gateway) Info(ctx context.Context) (*api.Info, error) { return result(g.c.CheckServer(ctx)) }
func (g *gateway) List(ctx context.Context, o app.ListOptions) (*api.Listing, error) {
	return result(g.c.List(ctx, client.ListOptions{Prefix: o.Prefix, Recursive: o.Recursive, Visibility: o.Visibility, Limit: o.Limit, Cursor: o.Cursor}))
}
func (g *gateway) Stat(ctx context.Context, p string) (*api.Object, error) {
	return result(g.c.Stat(ctx, p))
}
func (g *gateway) Upload(ctx context.Context, p string, r io.Reader, o app.UploadOptions) (*api.Object, bool, error) {
	obj, created, err := g.c.Upload(ctx, p, r, client.UploadOptions{Size: o.Size, ContentType: o.ContentType, Visibility: o.Visibility, Title: o.Title, Description: o.Description, Tags: o.Tags, Disposition: o.Disposition, ExpiresAt: o.ExpiresAt, ClearExpiry: o.ClearExpiry, SHA256: o.SHA256, CreateOnly: o.CreateOnly, IfRevision: o.IfRevision})
	return obj, created, classify(err)
}
func (g *gateway) Download(ctx context.Context, p string) (io.ReadCloser, error) {
	r, _, err := g.c.Download(ctx, p)
	return r, classify(err)
}
func (g *gateway) Patch(ctx context.Context, p string, v api.ObjectPatch, rev int64) (*api.Object, error) {
	return result(g.c.Patch(ctx, p, v, rev))
}
func (g *gateway) Move(ctx context.Context, v api.MoveRequest) (*api.MoveResponse, error) {
	return result(g.c.Move(ctx, v))
}
func (g *gateway) Delete(ctx context.Context, p string, o app.DeleteOptions) (*api.DeleteResponse, error) {
	return result(g.c.Delete(ctx, p, client.DeleteOptions{Recursive: o.Recursive, Purge: o.Purge, IfRevision: o.IfRevision}))
}
func (g *gateway) ListTrash(ctx context.Context, o app.TrashOptions) (*api.TrashList, error) {
	return result(g.c.ListTrash(ctx, client.TrashOptions{Prefix: o.Prefix, Path: o.Path, Limit: o.Limit, Cursor: o.Cursor}))
}
func (g *gateway) Restore(ctx context.Context, id string, v api.RestoreRequest) (*api.Object, error) {
	return result(g.c.Restore(ctx, id, v))
}
func (g *gateway) PurgeTrashEntry(ctx context.Context, id string) error {
	return classify(g.c.PurgeTrashEntry(ctx, id))
}
func (g *gateway) PurgeTrash(ctx context.Context, age string) (*api.PurgeResponse, error) {
	return result(g.c.PurgeTrash(ctx, age))
}
func (g *gateway) CreateLink(ctx context.Context, v api.CreateLinkRequest) (*api.Link, error) {
	return result(g.c.CreateLink(ctx, v))
}
func (g *gateway) ListLinks(ctx context.Context, o app.LinkOptions) (*api.LinkList, error) {
	return result(g.c.ListLinks(ctx, client.LinkListOptions{Path: o.Path, IncludeExpired: o.IncludeExpired, IncludeRevoked: o.IncludeRevoked}))
}
func (g *gateway) RevokeLink(ctx context.Context, id string) error {
	return classify(g.c.RevokeLink(ctx, id))
}
func (g *gateway) ListDirPolicies(ctx context.Context, o app.DirListOptions) (*api.DirPolicyList, error) {
	return result(g.c.ListDirPolicies(ctx, client.DirListOptions{Prefix: o.Prefix, Recursive: o.Recursive, Orphaned: o.Orphaned, Limit: o.Limit, Cursor: o.Cursor}))
}
func (g *gateway) ShowDir(ctx context.Context, p string) (*api.EffectiveDir, error) {
	return result(g.c.ShowDir(ctx, p))
}
func (g *gateway) PatchDir(ctx context.Context, p string, v api.DirPolicyPatch, rev int64) (*api.EffectiveDir, error) {
	return result(g.c.PatchDir(ctx, p, v, client.DirWriteOptions{IfRevision: rev}))
}
func (g *gateway) DeleteDir(ctx context.Context, p string, rev int64) error {
	return classify(g.c.DeleteDir(ctx, p, client.DirWriteOptions{IfRevision: rev}))
}
