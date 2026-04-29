package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/lib/rest"

	"golang.org/x/text/unicode/norm"
)

const (
	sharedstreamsAlbumPrefix    = "sharedstreams:"
	sharedstreamsResourcePrefix = "sharedstreams:"
)

// SharedAlbum represents a shared album from the sharedstreams API
type SharedAlbum struct {
	AlbumGUID    string
	Name         string
	Location     string // per-album URL base (for subscribed albums)
	Ctag         string // album change tag for webgetassets
	CreationDate time.Time
	IsPublic     bool
	SharingType  string // "owned" or "subscribed"
}

func (ps *PhotosService) getDSID() string {
	for _, cookie := range ps.client.Session.Cookies {
		if cookie.Name == "X-APPLE-WEBAUTH-USER" {
			for _, part := range strings.Split(cookie.Value, ":") {
				if strings.HasPrefix(part, "d=") {
					return part[2:]
				}
			}
		}
	}
	return ""
}

func (ps *PhotosService) getSharedstreamsBase() string {
	svc, ok := ps.client.Session.AccountInfo.Webservices["sharedstreams"]
	if !ok || svc.URL == "" {
		return ""
	}
	return svc.URL
}

// sharedstreamsURL returns the correct base URL for a shared album operation
// Owned albums use the dsid-based path; subscribed albums use the albumlocation
func (ps *PhotosService) sharedstreamsURL(sa *SharedAlbum, endpoint string) string {
	if sa.SharingType == "owned" {
		return fmt.Sprintf("%s/%s/sharedstreams/%s", ps.getSharedstreamsBase(), ps.getDSID(), endpoint)
	}
	return sa.Location + endpoint
}

func (ps *PhotosService) sharedstreamsRequest(ctx context.Context, reqURL string, data, response any) error {
	return ps.requestWithReauth(ctx, func() rest.Opts {
		return rest.Opts{
			Method:       "POST",
			RootURL:      reqURL,
			ExtraHeaders: ps.client.Session.GetHeaders(map[string]string{"Content-Type": "text/plain;charset=UTF-8"}),
		}
	}, data, response)
}

// GetSharedAlbums lists all shared albums via the sharedstreams API
func (ps *PhotosService) GetSharedAlbums(ctx context.Context) ([]*SharedAlbum, error) {
	base := ps.getSharedstreamsBase()
	dsid := ps.getDSID()
	if base == "" || dsid == "" {
		return nil, nil
	}

	reqURL := fmt.Sprintf("%s/%s/sharedstreams/webgetalbumslist", base, dsid)
	var response struct {
		Albums []struct {
			AlbumGUID     string `json:"albumguid"`
			AlbumLocation string `json:"albumlocation"`
			AlbumCtag     string `json:"albumctag"`
			SharingType   string `json:"sharingtype"`
			Attributes    struct {
				Name         string `json:"name"`
				IsPublic     any    `json:"isPublic"`
				CreationDate any    `json:"creationDate"`
			} `json:"attributes"`
		} `json:"albums"`
	}

	if err := ps.sharedstreamsRequest(ctx, reqURL, map[string]any{}, &response); err != nil {
		return nil, fmt.Errorf("sharedstreams webgetalbumslist: %w", err)
	}

	albums := make([]*SharedAlbum, 0, len(response.Albums))
	for _, a := range response.Albums {
		sa := &SharedAlbum{
			AlbumGUID:   a.AlbumGUID,
			Name:        a.Attributes.Name,
			Location:    a.AlbumLocation,
			Ctag:        a.AlbumCtag,
			SharingType: a.SharingType,
		}
		switch v := a.Attributes.IsPublic.(type) {
		case bool:
			sa.IsPublic = v
		case string:
			sa.IsPublic = v == "1"
		}
		switch v := a.Attributes.CreationDate.(type) {
		case float64:
			sa.CreationDate = time.UnixMilli(int64(v))
		case string:
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				sa.CreationDate = t
			}
		}
		albums = append(albums, sa)
	}
	return albums, nil
}

// getSharedAlbumPhotos fetches photos from a shared album via webgetassets
// Returns CPLMaster records with inline download URLs and filenames
func (ps *PhotosService) getSharedAlbumPhotos(ctx context.Context, sa *SharedAlbum) ([]*Photo, error) {
	reqURL := ps.sharedstreamsURL(sa, "webgetassets")

	var response struct {
		Records []json.RawMessage `json:"records"`
	}

	if err := ps.sharedstreamsRequest(ctx, reqURL, map[string]any{
		"albumguid": sa.AlbumGUID,
		"albumctag": sa.Ctag,
		"offset":    "0",
		"limit":     "200",
	}, &response); err != nil {
		return nil, fmt.Errorf("sharedstreams webgetassets: %w", err)
	}

	return parseSharedAlbumRecords(response.Records, sa)
}

// parseSharedAlbumRecords converts sharedstreams CPLMaster records to Photos
// The records have the same schema as CloudKit CPLMaster but include inline downloadURLs
func parseSharedAlbumRecords(records []json.RawMessage, sa *SharedAlbum) ([]*Photo, error) {
	var photos []*Photo
	for _, raw := range records {
		var rec struct {
			RecordName string `json:"recordName"`
			RecordType string `json:"recordType"`
			Fields     struct {
				FilenameEnc struct {
					Value string `json:"value"`
				} `json:"filenameEnc"`
				ResOriginalRes struct {
					Value struct {
						DownloadURL string `json:"downloadURL"`
						Size        int64  `json:"size"`
					} `json:"value"`
				} `json:"resOriginalRes"`
				ResJPEGMedRes struct {
					Value struct {
						DownloadURL string `json:"downloadURL"`
						Size        int64  `json:"size"`
					} `json:"value"`
				} `json:"resJPEGMedRes"`
				ResOriginalWidth struct {
					Value int `json:"value"`
				} `json:"resOriginalWidth"`
				ResOriginalHeight struct {
					Value int `json:"value"`
				} `json:"resOriginalHeight"`
				OriginalCreationDate struct {
					Value int64 `json:"value"`
				} `json:"originalCreationDate"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		if rec.RecordType != "CPLMaster" {
			continue
		}

		var filename string
		if rec.Fields.FilenameEnc.Value != "" {
			if decoded, err := base64.StdEncoding.DecodeString(rec.Fields.FilenameEnc.Value); err == nil {
				filename = norm.NFC.String(string(decoded))
			}
		}
		if filename == "" {
			filename = rec.RecordName + ".jpg"
		}

		downloadURL := rec.Fields.ResOriginalRes.Value.DownloadURL
		if downloadURL == "" {
			downloadURL = rec.Fields.ResJPEGMedRes.Value.DownloadURL
		}
		if downloadURL == "" {
			continue
		}

		// Sharedstreams records report size=0 in resource fields;
		// use -1 (unknown) so rclone skips the size check on transfer
		size := rec.Fields.ResOriginalRes.Value.Size
		if size == 0 {
			size = -1
		}

		// Encode albumGUID + sharingType + albumLocation in ResourceKey for download routing
		resourceInfo := sa.AlbumGUID + "|" + sa.SharingType + "|" + sa.Location
		photos = append(photos, &Photo{
			ID:          rec.RecordName,
			Filename:    filename,
			Size:        size,
			AssetDate:   rec.Fields.OriginalCreationDate.Value,
			AddedDate:   rec.Fields.OriginalCreationDate.Value,
			Width:       rec.Fields.ResOriginalWidth.Value,
			Height:      rec.Fields.ResOriginalHeight.Value,
			ResourceKey: sharedstreamsResourcePrefix + resourceInfo,
		})
	}
	return photos, nil
}

// LookupSharedAlbumDownloadURL fetches a fresh download URL for a sharedstreams photo
func (ps *PhotosService) LookupSharedAlbumDownloadURL(ctx context.Context, recordName, resourceInfo, _ string) (string, error) {
	info := strings.TrimPrefix(resourceInfo, sharedstreamsResourcePrefix)
	parts := strings.SplitN(info, "|", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid sharedstreams resource: %q", resourceInfo)
	}

	sa := &SharedAlbum{
		AlbumGUID:   parts[0],
		SharingType: parts[1],
		Location:    parts[2],
	}

	reqURL := ps.sharedstreamsURL(sa, "webgetassets")

	var response struct {
		Records []json.RawMessage `json:"records"`
	}

	if err := ps.sharedstreamsRequest(ctx, reqURL, map[string]any{
		"albumguid": sa.AlbumGUID,
		"offset":    "0",
		"limit":     "200",
	}, &response); err != nil {
		return "", fmt.Errorf("sharedstreams webgetassets: %w", err)
	}

	for _, raw := range response.Records {
		var rec struct {
			RecordName string `json:"recordName"`
			RecordType string `json:"recordType"`
			Fields     struct {
				ResOriginalRes struct {
					Value struct {
						DownloadURL string `json:"downloadURL"`
					} `json:"value"`
				} `json:"resOriginalRes"`
				ResJPEGMedRes struct {
					Value struct {
						DownloadURL string `json:"downloadURL"`
					} `json:"value"`
				} `json:"resJPEGMedRes"`
			} `json:"fields"`
		}
		if json.Unmarshal(raw, &rec) != nil || rec.RecordName != recordName {
			continue
		}
		if url := rec.Fields.ResOriginalRes.Value.DownloadURL; url != "" {
			return url, nil
		}
		if url := rec.Fields.ResJPEGMedRes.Value.DownloadURL; url != "" {
			return url, nil
		}
	}
	return "", fmt.Errorf("record %q not found in shared album %q", recordName, sa.AlbumGUID)
}

// IsSharedstreamsResource returns true if the resourceKey indicates a sharedstreams photo
func IsSharedstreamsResource(resourceKey string) bool {
	return strings.HasPrefix(resourceKey, sharedstreamsResourcePrefix)
}

// getSharedstreamsPhotos fetches photos for a sharedstreams-backed album
func (album *Album) getSharedstreamsPhotos(ctx context.Context) ([]*Photo, error) {
	album.mu.Lock()
	if album.photoCache != nil {
		result := make([]*Photo, 0, len(album.photoCache))
		for _, p := range album.photoCache {
			result = append(result, p)
		}
		album.mu.Unlock()
		return result, nil
	}
	album.mu.Unlock()

	sa := albumToSharedAlbum(album)
	photos, err := album.lib.service.getSharedAlbumPhotos(ctx, sa)
	if err != nil {
		return nil, err
	}

	deduplicateFilenames(photos)

	album.mu.Lock()
	album.photoCache = buildPhotoCache(photos)
	album.mu.Unlock()

	return photos, nil
}

// albumToSharedAlbum reconstructs a SharedAlbum from the encoded Album fields
func albumToSharedAlbum(album *Album) *SharedAlbum {
	info := strings.TrimPrefix(album.ObjectType, sharedstreamsAlbumPrefix)
	parts := strings.SplitN(info, "|", 3)
	sa := &SharedAlbum{AlbumGUID: info}
	if len(parts) == 3 {
		sa.AlbumGUID = parts[0]
		sa.SharingType = parts[1]
		sa.Location = parts[2]
	}
	sa.Ctag = album.Direction // reuse Direction field for ctag storage
	return sa
}

// buildSharedAlbumsForLibrary creates Album entries from cached sharedstreams data
func (ps *PhotosService) buildSharedAlbumsForLibrary(ctx context.Context, lib *Library) (map[string]*Album, error) {
	ps.mu.Lock()
	sharedAlbums := ps.sharedAlbums
	ps.mu.Unlock()

	if len(sharedAlbums) == 0 {
		return map[string]*Album{}, nil
	}

	albums := make(map[string]*Album, len(sharedAlbums))
	for _, sa := range sharedAlbums {
		name := sa.Name
		if name == "" {
			name = sa.AlbumGUID
		}
		name = path.Base(name)

		// Encode album identity in ObjectType for later retrieval
		objectType := sharedstreamsAlbumPrefix + sa.AlbumGUID + "|" + sa.SharingType + "|" + sa.Location

		albums[name] = &Album{
			Name:       name,
			ObjectType: objectType,
			Direction:  sa.Ctag, // store albumctag for webgetassets
			lib:        lib,
		}
	}
	return albums, nil
}
