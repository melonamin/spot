package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var fileIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

type LocalFileStore struct {
	root string
}

func NewLocalFileStore(root string) (*LocalFileStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create uploads dir: %w", err)
	}
	return &LocalFileStore{root: root}, nil
}

func (f *LocalFileStore) Put(_ context.Context, site, filename, contentType string, r io.Reader, _ int64) (StoredFile, error) {
	if !siteNameRe.MatchString(site) {
		return StoredFile{}, fmt.Errorf("invalid file site")
	}
	id, err := newFileID()
	if err != nil {
		return StoredFile{}, err
	}
	name := sanitizeFilename(filename)
	dir := filepath.Join(f.root, site, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return StoredFile{}, fmt.Errorf("create upload dir: %w", err)
	}
	path := filepath.Join(dir, name)
	written, err := writeFileAtomic(path, r)
	if err != nil {
		return StoredFile{}, fmt.Errorf("store file %s/%s/%s: %w", site, id, name, err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return StoredFile{
		ID:          id,
		Name:        name,
		Size:        written,
		ContentType: contentType,
		URL:         "/api/files/" + site + "/" + id + "/" + name,
	}, nil
}

func (f *LocalFileStore) Get(_ context.Context, site, id, name string) (io.ReadCloser, string, error) {
	if !siteNameRe.MatchString(site) || !fileIDRe.MatchString(id) || sanitizeFilename(name) != name {
		return nil, "", ErrNotFound
	}
	file, err := os.OpenInRoot(f.root, filepath.Join(site, id, name))
	if os.IsNotExist(err) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("open file %s/%s/%s: %w", site, id, name, err)
	}
	contentType := sniffContentType(file)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, "", fmt.Errorf("seek file %s/%s/%s: %w", site, id, name, err)
	}
	return file, contentType, nil
}

func (f *LocalFileStore) RemoveSite(_ context.Context, site string) error {
	if !siteNameRe.MatchString(site) {
		return fmt.Errorf("invalid file site")
	}
	if err := os.RemoveAll(filepath.Join(f.root, site)); err != nil {
		return fmt.Errorf("remove uploads for %s: %w", site, err)
	}
	return nil
}

type LocalSiteStore struct {
	root string
}

func NewLocalSiteStore(root string) (*LocalSiteStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create sites dir: %w", err)
	}
	return &LocalSiteStore{root: root}, nil
}

func (s *LocalSiteStore) Put(_ context.Context, site, path, _ string, data []byte) error {
	if !siteNameRe.MatchString(site) || !validSitePath(path) {
		return fmt.Errorf("invalid site file path")
	}
	full := filepath.Join(s.root, site, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create site file dir: %w", err)
	}
	if _, err := writeFileAtomic(full, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("store site file %s/%s: %w", site, path, err)
	}
	return nil
}

func (s *LocalSiteStore) List(_ context.Context, site string) ([]string, error) {
	if !siteNameRe.MatchString(site) {
		return nil, fmt.Errorf("invalid site name")
	}
	root := filepath.Join(s.root, site)
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list site %s: %w", site, err)
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *LocalSiteStore) Open(_ context.Context, site, path string) (io.ReadCloser, SiteFileInfo, error) {
	if !siteNameRe.MatchString(site) || !validSitePath(path) {
		return nil, SiteFileInfo{}, ErrNotFound
	}
	file, err := os.OpenInRoot(s.root, filepath.Join(site, filepath.FromSlash(path)))
	if os.IsNotExist(err) {
		return nil, SiteFileInfo{}, ErrNotFound
	}
	if err != nil {
		return nil, SiteFileInfo{}, fmt.Errorf("open site file %s/%s: %w", site, path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, SiteFileInfo{}, fmt.Errorf("stat site file %s/%s: %w", site, path, err)
	}
	if info.IsDir() {
		file.Close()
		return nil, SiteFileInfo{}, ErrNotFound
	}
	return file, SiteFileInfo{LastModified: info.ModTime()}, nil
}

func (s *LocalSiteStore) Remove(_ context.Context, site, path string) error {
	if !siteNameRe.MatchString(site) || !validSitePath(path) {
		return fmt.Errorf("invalid site file path")
	}
	if err := os.Remove(filepath.Join(s.root, site, filepath.FromSlash(path))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove site file %s/%s: %w", site, path, err)
	}
	pruneEmptyDirs(filepath.Join(s.root, site), filepath.Dir(filepath.Join(s.root, site, filepath.FromSlash(path))))
	return nil
}

func validSitePath(p string) bool {
	return validDownloadPath(p)
}

func writeFileAtomic(path string, r io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}
	return n, nil
}

func sniffContentType(file *os.File) string {
	sniff := make([]byte, 512)
	n, _ := io.ReadFull(file, sniff)
	if n == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(sniff[:n])
}

func pruneEmptyDirs(root, dir string) {
	root = filepath.Clean(root)
	dir = filepath.Clean(dir)
	for strings.HasPrefix(dir, root) && dir != root {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
