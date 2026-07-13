// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Package importer runs durable background imports that pre-warm the
// Hugging Face proxy cache.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pulsys-io/pulsys/internal/hfhub"
)

type Downloader interface {
	Download(ctx context.Context, spec ImportSpec, progress func(ImportProgress)) error
}

type ImportSpec struct {
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision"`
	RepoType string `json:"repo_type"`
}

type ImportProgress struct {
	Phase                 string    `json:"phase"`
	FilesTotal            int       `json:"files_total,omitempty"`
	FilesDone             int       `json:"files_done,omitempty"`
	BytesTotal            int64     `json:"bytes_total,omitempty"`
	BytesDone             int64     `json:"bytes_done,omitempty"`
	CurrentFile           string    `json:"current_file,omitempty"`
	CurrentFileBytesTotal int64     `json:"current_file_bytes_total,omitempty"`
	CurrentFileBytesDone  int64     `json:"current_file_bytes_done,omitempty"`
	DownloadBPS           float64   `json:"download_bps,omitempty"`
	Message               string    `json:"message,omitempty"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func parseSpec(raw json.RawMessage) (ImportSpec, error) {
	var spec ImportSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, err
	}
	spec.RepoID = strings.TrimSpace(spec.RepoID)
	spec.Revision = strings.TrimSpace(spec.Revision)
	spec.RepoType = strings.TrimSpace(spec.RepoType)
	if spec.RepoID == "" {
		return spec, errors.New("repo_id required")
	}
	if spec.Revision == "" {
		spec.Revision = "main"
	}
	if spec.RepoType == "" {
		spec.RepoType = "models"
	}
	return spec, nil
}

type HFCacheDownloader struct {
	Endpoint   string
	Token      string
	Workers    int
	RangeChunk int64
	HTTP       *http.Client
}

func (d HFCacheDownloader) Download(ctx context.Context, spec ImportSpec, progress func(ImportProgress)) error {
	endpoint := strings.TrimRight(d.Endpoint, "/")
	if endpoint == "" {
		return errors.New("importer: endpoint required")
	}
	workers := d.Workers
	if workers <= 0 {
		workers = 32
	}
	client := &hfhub.Client{
		Base:  endpoint,
		Token: strings.TrimSpace(d.Token),
		HTTP:  d.HTTP,
	}
	if client.HTTP == nil {
		client.HTTP = &http.Client{
			Transport: hfhub.NewTransport(workers),
			Timeout:   0,
		}
	}
	rt, err := repoType(spec.RepoType)
	if err != nil {
		return err
	}
	var lastDone int64
	opts := hfhub.DownloadOpts{
		Sink:       io.Discard,
		Workers:    workers,
		Revision:   spec.Revision,
		RangeChunk: d.RangeChunk,
		RepoType:   rt,
		Progress: func(done, total int64) {
			msg := "downloading"
			if done == lastDone && done > 0 {
				msg = "cache hit"
			}
			lastDone = done
			progress(ImportProgress{
				Phase:      "downloading",
				BytesDone:  done,
				BytesTotal: total,
				Message:    msg,
			})
		},
	}
	_, err = client.Download(ctx, spec.RepoID, opts)
	return err
}

func repoType(s string) (hfhub.RepoType, error) {
	switch strings.TrimSpace(s) {
	case "", "models", "model":
		return hfhub.RepoTypeModel, nil
	case "datasets", "dataset":
		return hfhub.RepoTypeDataset, nil
	case "spaces", "space":
		return hfhub.RepoTypeSpace, nil
	default:
		return "", fmt.Errorf("unsupported repo_type %q", s)
	}
}
