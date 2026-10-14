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
	register(r, "prepare_file_upload", "Prepare a single-use HTTP upload of any binary file from your computing environment or chat attachment. Supply exact raw byte size and SHA-256; PUT the original bytes to the returned URL using its headers. Applies all metadata while creating a NEW file, private by default. Never overwrites. This preparation does not upload bytes.", object(schema{"path": filePath(), "size": integer("Exact size of the original file in bytes, including zero for an empty file.", 0, 0), "sha256": schema{"type": "string", "pattern": "^[a-fA-F0-9]{64}$", "description": "SHA-256 of the original file, computed in your environment."}, "metadata": metadata()}, "path", "size", "sha256"), func(ctx context.Context, _ *app.Workspace, a transfers.UploadRequest) (any, error) {
		return r.transferService.PrepareUpload(ctx, a)
	})
	register(r, "prepare_file_download", "Get a short-lived browser-usable HTTP URL for the original bytes of any file: images, PDF, archives, Office files and arbitrary binary. Read-only permission is sufficient. Download into your computing environment, verify SHA-256, then process it or attach it to your reply. Private visibility is preserved; the capability expires and remains subject to owner authorization. Supports Range requests and bounded retries.", object(schema{"path": filePath()}, "path"), func(ctx context.Context, _ *app.Workspace, a pathArgs) (any, error) {
		return r.transferService.PrepareDownload(ctx, a.Path)
	})
	register(r, "get_file_transfer_status", "Inspect your HTTP transfer after a lost response: prepared, uploading, succeeded, failed, uncertain, or expired. Returns the verified upload receipt when available. An uncertain result can include matching destination metadata for recovery; never blindly overwrite or repeat a mutation. Capability secrets are not returned.", object(schema{"transfer_id": str("Opaque transfer_id from prepare_file_upload or prepare_file_download.")}, "transfer_id"), func(ctx context.Context, _ *app.Workspace, a transferStatusArgs) (any, error) {
		return r.transferService.Status(ctx, a.TransferID)
	})
}
