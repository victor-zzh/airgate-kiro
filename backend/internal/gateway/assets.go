package gateway

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed webdist/*
var webDistFS embed.FS

func (g *KiroGateway) GetWebAssets() map[string][]byte {
	if assets := loadDevWebAssets(); len(assets) > 0 {
		return assets
	}

	assets := make(map[string][]byte)
	_ = fs.WalkDir(webDistFS, "webdist", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		content, err := webDistFS.ReadFile(path)
		if err != nil {
			return nil
		}
		relPath := strings.TrimPrefix(path, "webdist/")
		assets[relPath] = content
		return nil
	})
	return assets
}

func loadDevWebAssets() map[string][]byte {
	for _, dir := range []string{
		filepath.Join("..", "web", "dist"),
		filepath.Join("web", "dist"),
	} {
		if assets := loadAssetsFromDir(dir); len(assets) > 0 {
			return assets
		}
	}
	return nil
}

func loadAssetsFromDir(root string) map[string][]byte {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	assets := make(map[string][]byte)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		assets[filepath.ToSlash(relPath)] = content
		return nil
	})

	if len(assets) == 0 {
		return nil
	}
	return assets
}
