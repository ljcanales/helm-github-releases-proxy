package local

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type archive struct {
	name     string
	version  string
	filename string
	digest   string
	created  time.Time
}

var filenamePattern = regexp.MustCompile(`^(?P<name>[^/\\]+)-(?P<version>v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\.tgz$`)

func scan(directory string, logger *slog.Logger) ([]archive, int, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, 0, err
	}
	archives := make([]archive, 0)
	skipped := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tgz") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
			skipped++
			logger.Warn("skipping local chart", "source_path", directory, "filename", entry.Name(), "error", "not a regular file")
			continue
		}
		chartArchive, err := inspect(filepath.Join(directory, entry.Name()), entry.Name(), info.ModTime())
		if err != nil {
			skipped++
			logger.Warn("skipping local chart", "source_path", directory, "filename", entry.Name(), "error", err)
			continue
		}
		archives = append(archives, chartArchive)
	}
	return archives, skipped, nil
}

func inspect(path, filename string, modified time.Time) (archive, error) {
	match := filenamePattern.FindStringSubmatch(filename)
	if match == nil {
		return archive{}, fmt.Errorf("filename does not match '<chart-name>-<version>.tgz'")
	}
	filenameName := match[1]
	filenameVersion := normalizeVersion(match[2])
	metadata, err := readMetadata(path)
	if err != nil {
		return archive{}, err
	}
	if metadata.Name != filenameName || normalizeVersion(metadata.Version) != filenameVersion {
		return archive{}, fmt.Errorf("filename metadata does not match Chart.yaml")
	}
	file, err := os.Open(path)
	if err != nil {
		return archive{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return archive{}, err
	}
	return archive{name: filenameName, version: filenameVersion, filename: filename, digest: hex.EncodeToString(hash.Sum(nil)), created: modified}, nil
}

type chartMetadata struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

func readMetadata(path string) (chartMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return chartMetadata{}, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return chartMetadata{}, fmt.Errorf("invalid gzip archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return chartMetadata{}, fmt.Errorf("invalid tar archive: %w", err)
		}
		if !safeMetadataPath(header.Name) || header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			continue
		}
		if filepath.Base(filepath.ToSlash(header.Name)) != "Chart.yaml" || header.Typeflag != tar.TypeReg {
			continue
		}
		var metadata chartMetadata
		if err := yaml.NewDecoder(tarReader).Decode(&metadata); err != nil {
			return chartMetadata{}, fmt.Errorf("invalid Chart.yaml: %w", err)
		}
		if metadata.Name == "" || metadata.Version == "" {
			return chartMetadata{}, fmt.Errorf("Chart.yaml requires name and version")
		}
		return metadata, nil
	}
	return chartMetadata{}, fmt.Errorf("archive does not contain a safe Chart.yaml")
}

func safeMetadataPath(name string) bool {
	name = filepath.ToSlash(name)
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func normalizeVersion(version string) string { return strings.TrimPrefix(version, "v") }
