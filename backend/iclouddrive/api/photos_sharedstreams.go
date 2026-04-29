package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/rest"

	"golang.org/x/text/unicode/norm"
)

const (
	sharedstreamsAlbumPrefix    = "sharedstreams:"
	sharedstreamsResourcePrefix = "sharedstreams:"
	sharedstreamsPageSize       = 200
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

// fetchSharedAlbumRecords fetches all records from a shared album with pagination
func (ps *PhotosService) fetchSharedAlbumRecords(ctx context.Context, sa *SharedAlbum) ([]json.RawMessage, error) {
	reqURL := ps.sharedstreamsURL(sa, "webgetassets")
	var allRecords []json.RawMessage

	for offset := 0; ; offset += sharedstreamsPageSize {
		var response struct {
			Records []json.RawMessage `json:"records"`
		}
		body := map[string]any{
			"albumguid": sa.AlbumGUID,
			"offset":    fmt.Sprintf("%d", offset),
			"limit":     fmt.Sprintf("%d", sharedstreamsPageSize),
		}
		if sa.Ctag != "" {
			body["albumctag"] = sa.Ctag
		}
		if err := ps.sharedstreamsRequest(ctx, reqURL, body, &response); err != nil {
			return nil, fmt.Errorf("sharedstreams webgetassets: %w", err)
		}
		if len(response.Records) == 0 {
			break
		}
		allRecords = append(allRecords, response.Records...)
	}
	return allRecords, nil
}

// getSharedAlbumPhotos fetches photos from a shared album via webgetassets
func (ps *PhotosService) getSharedAlbumPhotos(ctx context.Context, sa *SharedAlbum) ([]*Photo, map[string]string, error) {
	records, err := ps.fetchSharedAlbumRecords(ctx, sa)
	if err != nil {
		return nil, nil, err
	}
	return parseSharedAlbumRecords(records, sa)
}

// parseSharedAlbumRecords converts sharedstreams CPLMaster records to Photos
// Also matches CPLAsset metadata (favorites, hidden) when present
// Returns photos and a recordName->downloadURL map for URL caching
func parseSharedAlbumRecords(records []json.RawMessage, sa *SharedAlbum) ([]*Photo, map[string]string, error) {
	var photos []*Photo
	urls := make(map[string]string)

	type assetMeta struct {
		masterID   string
		isFavorite bool
		isHidden   bool
	}
	var assets []assetMeta

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
				MasterRef struct {
					Value struct {
						RecordName string `json:"recordName"`
					} `json:"value"`
				} `json:"masterRef"`
				IsFavorite struct {
					Value int `json:"value"`
				} `json:"isFavorite"`
				IsHidden struct {
					Value int `json:"value"`
				} `json:"isHidden"`
			} `json:"fields"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}

		switch rec.RecordType {
		case "CPLMaster":
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

			size := rec.Fields.ResOriginalRes.Value.Size
			if size == 0 {
				size = -1
			}

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
			urls[rec.RecordName] = downloadURL

		case "CPLAsset":
			masterID := rec.Fields.MasterRef.Value.RecordName
			if masterID != "" {
				assets = append(assets, assetMeta{
					masterID:   masterID,
					isFavorite: rec.Fields.IsFavorite.Value == 1,
					isHidden:   rec.Fields.IsHidden.Value == 1,
				})
			}
		}
	}

	if len(assets) > 0 {
		byID := make(map[string]*Photo, len(photos))
		for _, p := range photos {
			byID[p.ID] = p
		}
		for _, a := range assets {
			if p, ok := byID[a.masterID]; ok {
				p.IsFavorite = a.isFavorite
				p.IsHidden = a.isHidden
			}
		}
	}

	return photos, urls, nil
}

// LookupSharedAlbumDownloadURL returns a download URL for a sharedstreams photo,
// using cached URLs from the initial listing when available
func (ps *PhotosService) LookupSharedAlbumDownloadURL(ctx context.Context, recordName, resourceInfo, _ string) (string, error) {
	if url, ok := ps.ssURLs.Load(recordName); ok {
		return url.(string), nil
	}

	sa, err := resourceInfoToSharedAlbum(resourceInfo)
	if err != nil {
		return "", err
	}

	records, err := ps.fetchSharedAlbumRecords(ctx, sa)
	if err != nil {
		return "", err
	}

	var result string
	for _, raw := range records {
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
		if json.Unmarshal(raw, &rec) != nil || rec.RecordType != "CPLMaster" {
			continue
		}
		url := rec.Fields.ResOriginalRes.Value.DownloadURL
		if url == "" {
			url = rec.Fields.ResJPEGMedRes.Value.DownloadURL
		}
		if url != "" {
			ps.ssURLs.Store(rec.RecordName, url)
			if rec.RecordName == recordName {
				result = url
			}
		}
	}
	if result == "" {
		return "", fmt.Errorf("record %q not found in shared album %q", recordName, sa.AlbumGUID)
	}
	return result, nil
}

func resourceInfoToSharedAlbum(resourceInfo string) (*SharedAlbum, error) {
	info := strings.TrimPrefix(resourceInfo, sharedstreamsResourcePrefix)
	parts := strings.SplitN(info, "|", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid sharedstreams resource: %q", resourceInfo)
	}
	return &SharedAlbum{
		AlbumGUID:   parts[0],
		SharingType: parts[1],
		Location:    parts[2],
	}, nil
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

	cacheDir := album.lib.service.sharedstreamsCacheDir()
	if sa.Ctag != "" {
		if cached := loadSharedAlbumDiskCache(cacheDir, sa.AlbumGUID, sa.Ctag); cached != nil {
			deduplicateFilenames(cached)
			album.mu.Lock()
			album.photoCache = buildPhotoCache(cached)
			album.mu.Unlock()
			fs.Debugf(nil, "iclouddrive photos: %d shared photos from cache for %s", len(cached), album.Name)
			return cached, nil
		}
	}

	photos, urls, err := album.lib.service.getSharedAlbumPhotos(ctx, sa)
	if err != nil {
		return nil, err
	}
	for id, url := range urls {
		album.lib.service.ssURLs.Store(id, url)
	}

	if sa.Ctag != "" {
		saveSharedAlbumDiskCache(cacheDir, sa.AlbumGUID, sa.Ctag, photos)
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

	nameCounts := make(map[string]int, len(sharedAlbums))
	for _, sa := range sharedAlbums {
		name := sa.Name
		if name == "" {
			name = sa.AlbumGUID
		}
		nameCounts[path.Base(name)]++
	}

	albums := make(map[string]*Album, len(sharedAlbums))
	for _, sa := range sharedAlbums {
		name := sa.Name
		if name == "" {
			name = sa.AlbumGUID
		}
		name = path.Base(name)
		if nameCounts[name] > 1 {
			name = name + "_" + sa.AlbumGUID
		}

		objectType := sharedstreamsAlbumPrefix + sa.AlbumGUID + "|" + sa.SharingType + "|" + sa.Location

		albums[name] = &Album{
			Name:       name,
			ObjectType: objectType,
			Direction:  sa.Ctag,
			lib:        lib,
		}
	}
	return albums, nil
}

// sharedstreamsCacheDir returns the disk cache directory for sharedstreams data
func (ps *PhotosService) sharedstreamsCacheDir() string {
	return filepath.Join(ps.client.CacheDir(), "sharedstreams")
}

type sharedAlbumDiskCache struct {
	Ctag   string   `json:"ctag"`
	Photos []*Photo `json:"photos"`
}

func loadSharedAlbumDiskCache(cacheDir, albumGUID, currentCtag string) []*Photo {
	data, err := os.ReadFile(filepath.Join(cacheDir, albumGUID+".json"))
	if err != nil {
		return nil
	}
	var cache sharedAlbumDiskCache
	if json.Unmarshal(data, &cache) != nil || cache.Ctag != currentCtag {
		return nil
	}
	return cache.Photos
}

func saveSharedAlbumDiskCache(cacheDir, albumGUID, ctag string, photos []*Photo) {
	saveJSONCache(cacheDir, albumGUID+".json", &sharedAlbumDiskCache{
		Ctag:   ctag,
		Photos: photos,
	})
}

// pollSharedstreamsForChanges checks if any shared album ctags changed
func (ps *PhotosService) pollSharedstreamsForChanges(ctx context.Context) bool {
	ps.mu.Lock()
	old := ps.sharedAlbums
	ps.mu.Unlock()

	if len(old) == 0 {
		return false
	}

	fresh, err := ps.GetSharedAlbums(ctx)
	if err != nil {
		return false
	}

	if !sharedAlbumsChanged(old, fresh) {
		return false
	}

	ps.mu.Lock()
	ps.sharedAlbums = fresh
	lib := ps.libraries["Shared Albums"]
	ps.mu.Unlock()

	if lib != nil {
		lib.mu.Lock()
		for _, album := range lib.albums {
			album.mu.Lock()
			album.photoCache = nil
			album.mu.Unlock()
		}
		lib.albums = make(map[string]*Album)
		lib.mu.Unlock()
	}

	fs.Debugf(nil, "iclouddrive photos: ChangeNotify detected changes in shared albums")
	return true
}

func sharedAlbumsChanged(old, fresh []*SharedAlbum) bool {
	if len(old) != len(fresh) {
		return true
	}
	oldCtags := make(map[string]string, len(old))
	for _, sa := range old {
		oldCtags[sa.AlbumGUID] = sa.Ctag
	}
	for _, sa := range fresh {
		if oldCtags[sa.AlbumGUID] != sa.Ctag {
			return true
		}
	}
	return false
}
