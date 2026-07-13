// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package mockhub

// HF Hub wire types. Names match the actual API as observed in
// huggingface_hub and the upstream HTTP responses. JSON tags use the
// camelCase / snake_case the real Hub returns so clients parse them
// verbatim.

// modelInfoResponse mirrors GET /api/models/{repo}.
type modelInfoResponse struct {
	ID       string         `json:"id"`
	ModelID  string         `json:"modelId"`
	SHA      string         `json:"sha"`
	Tags     []string       `json:"tags"`
	Siblings []siblingEntry `json:"siblings"`
	Private  bool           `json:"private"`
}

type siblingEntry struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
	BlobID    string `json:"blob_id"`
	LFS       *lfs   `json:"lfs,omitempty"`
}

// treeEntry mirrors one entry of GET /api/models/{repo}/tree/{rev}.
type treeEntry struct {
	Type string `json:"type"` // "file" or "directory"
	OID  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  *lfs   `json:"lfs,omitempty"`
}

// lfs is the inline LFS pointer block HF embeds in tree/paths-info
// responses for LFS-tracked files.
type lfs struct {
	OID         string `json:"oid"`
	Size        int64  `json:"size"`
	PointerSize int    `json:"pointerSize"`
}

// pathsInfoRequest mirrors POST /api/models/{repo}/paths-info/{rev}.
type pathsInfoRequest struct {
	Paths     []string `json:"paths"`
	Expand    bool     `json:"expand,omitempty"`
	Recursive bool     `json:"recursive,omitempty"`
}

type pathsInfoEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Type       string `json:"type"`
	OID        string `json:"oid"`
	LFS        *lfs   `json:"lfs,omitempty"`
	LastCommit *struct {
		ID   string `json:"id"`
		Date string `json:"date"`
	} `json:"lastCommit,omitempty"`
}

// preuploadRequest mirrors POST /api/models/{repo}/preupload/{rev}.
type preuploadRequest struct {
	Files         []preuploadFileRequest `json:"files"`
	GitAttributes string                 `json:"gitAttributes,omitempty"`
}

type preuploadFileRequest struct {
	Path   string `json:"path"`
	Sample string `json:"sample"` // base64-encoded sample for dedup
	Size   int64  `json:"size"`
}

type preuploadResponse struct {
	Files []preuploadFileResponse `json:"files"`
}

type preuploadFileResponse struct {
	Path         string `json:"path"`
	UploadMode   string `json:"uploadMode"`   // "regular" | "lfs"
	ShouldIgnore bool   `json:"shouldIgnore"` // true when already present (dedup hit)
	OID          string `json:"oid,omitempty"`
}

// commitResponse mirrors POST /api/models/{repo}/commit/{rev}.
//
// The request body is NDJSON; see decodeCommitNDJSON in handlers.go.
type commitResponse struct {
	Success   bool   `json:"success"`
	CommitOID string `json:"commitOid"`
	CommitURL string `json:"commitUrl"`
	PRURL     string `json:"prUrl,omitempty"`
}

// lfsBatchRequest mirrors POST /{repo}.git/info/lfs/objects/batch.
type lfsBatchRequest struct {
	Operation string        `json:"operation"` // "upload" or "download"
	Transfers []string      `json:"transfers"` // ["basic","multipart"]
	Ref       *lfsBatchRef  `json:"ref,omitempty"`
	Objects   []lfsBatchObj `json:"objects"`
	HashAlgo  string        `json:"hash_algo,omitempty"`
}

type lfsBatchRef struct {
	Name string `json:"name"`
}

type lfsBatchObj struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchResponse struct {
	Transfer string                `json:"transfer"`
	Objects  []lfsBatchObjResponse `json:"objects"`
	HashAlgo string                `json:"hash_algo,omitempty"`
}

type lfsBatchObjResponse struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsObjectError      `json:"error,omitempty"`
	// AuthenticatedAction is the form HF sometimes returns; alias
	// kept for compatibility with multipart adapters. Real Hub only
	// uses Actions; we don't populate this field.
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lfsVerifyRequest mirrors POST /{repo}.git/info/lfs/verify.
type lfsVerifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}
