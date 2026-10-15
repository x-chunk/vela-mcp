package mcpserver

import (
	"context"

	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/transfers"
)

type transferStatusArgs struct {
	TransferID string `json:"transfer_id"`
}

// transfers adds the HTTP control plane only when the host configured a broker.
// The general registrar still applies each tool's enable flag and resolves the
// caller separately for every invocation, exactly like existing Vela workflows.
func (r *registrar) transfers() {
	if r.transferService == nil {
		return
	}
	register(r, "prepare_file_upload", "Prepare an HTTP upload for a file accessible in your computing environment. Compute its exact byte size and SHA-256, then PUT the original bytes with the returned URL and headers. Preparation alone uploads nothing. Creates a new file with all supplied metadata, private by default; never overwrites.", object(schema{"path": filePath(), "size": integer("Exact size of the original file in bytes, including zero for an empty file.", 0, 0), "sha256": schema{"type": "string", "pattern": "^[a-fA-F0-9]{64}$", "description": "SHA-256 of the original file, computed in your environment."}, "metadata": metadata()}, "path", "size", "sha256"), func(ctx context.Context, _ *app.Workspace, a transfers.UploadRequest) (any, error) {
		return r.transferService.PrepareUpload(ctx, a)
	})
	register(r, "prepare_file_download", "Get a temporary HTTP URL for any file's original bytes, including PDF, images, archives and Office files. GET into your computing environment and verify SHA-256, then process it or attach it if the client supports attachments. Read-only access is sufficient; visibility is unchanged. Anyone with the URL can download until it expires or authorization is revoked.", object(schema{"path": filePath()}, "path"), func(ctx context.Context, _ *app.Workspace, a pathArgs) (any, error) {
		return r.transferService.PrepareDownload(ctx, a.Path)
	})
	register(r, "get_file_transfer_status", "Recover after a lost upload response using its transfer_id. A succeeded status includes the verified receipt; an uncertain status requires inspection before any new mutation. Matching destination bytes alone do not prove metadata was applied. A successful PUT receipt needs no extra status call. Capability secrets are omitted.", object(schema{"transfer_id": str("Opaque transfer_id from prepare_file_upload or prepare_file_download.")}, "transfer_id"), func(ctx context.Context, _ *app.Workspace, a transferStatusArgs) (any, error) {
		return r.transferService.Status(ctx, a.TransferID)
	})
}
