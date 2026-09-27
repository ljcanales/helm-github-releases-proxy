package local_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm-github-releases-proxy/internal/repository"
	"helm-github-releases-proxy/internal/source/local"
)

func TestSourceDiscoversAndOpensEligibleChart(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-chart-v1.2.3.tgz"
	path := filepath.Join(directory, filename)
	writeChart(t, path, "demo-chart", "1.2.3", "payload")
	if err := os.WriteFile(filepath.Join(directory, "bad.tgz"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	source := local.New("local", directory, slog.Default())
	contribution, err := source.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contribution.IndexedCount != 1 || contribution.SkippedCount != 1 || len(contribution.ChartVersions) != 1 {
		t.Fatalf("contribution = %#v", contribution)
	}
	discovered := contribution.ChartVersions[0]
	if discovered.Version.Name != "demo-chart" || discovered.Version.Version != "1.2.3" || discovered.Version.Created != "2026-08-30T12:00:00Z" {
		t.Fatalf("version = %#v", discovered.Version)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if discovered.Version.Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %q", discovered.Version.Digest)
	}
	if len(discovered.Packages) != 1 || discovered.Packages[0].AdvertisedPath != "charts/local/"+filename {
		t.Fatalf("packages = %#v", discovered.Packages)
	}

	pkg, err := source.OpenPackage(context.Background(), discovered.Packages[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	defer pkg.Body.Close()
	got, err := io.ReadAll(pkg.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) || pkg.Filename != filename {
		t.Fatal("opened package differs from discovered archive")
	}
}

func TestSourceRejectsInvalidReferencesAndReplacedSymlinks(t *testing.T) {
	directory := t.TempDir()
	filename := "demo-1.2.3.tgz"
	path := filepath.Join(directory, filename)
	writeChart(t, path, "demo", "1.2.3", "original")
	source := local.New("local", directory, slog.Default())

	for _, reference := range []repository.PackageReference{
		{Source: "other", Key: filename},
		{Source: "local", Key: "../" + filename},
		{Source: "local", Key: path},
		{Source: "local", Key: "nested/" + filename},
	} {
		if pkg, err := source.OpenPackage(context.Background(), reference); err == nil {
			pkg.Body.Close()
			t.Fatalf("opened invalid reference: %#v", reference)
		}
	}

	target := filepath.Join(t.TempDir(), filename)
	writeChart(t, target, "demo", "1.2.3", "replacement")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if pkg, err := source.OpenPackage(context.Background(), repository.PackageReference{Source: "local", Key: filename}); err == nil {
		pkg.Body.Close()
		t.Fatal("opened a symlink that replaced a published file")
	}
}

func TestSourceRequiresMatchingSafeChartMetadata(t *testing.T) {
	directory := t.TempDir()
	writeChart(t, filepath.Join(directory, "demo-1.2.3.tgz"), "other", "1.2.3", "mismatch")
	writeChart(t, filepath.Join(directory, "demo-1.2.4.tgz"), "demo", "v1.2.3", "mismatch")

	contribution, err := local.New("local", directory, slog.Default()).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contribution.IndexedCount != 0 || contribution.SkippedCount != 2 || len(contribution.ChartVersions) != 0 {
		t.Fatalf("contribution = %#v", contribution)
	}
}

func TestSourceDiscoversAnEmptyDirectory(t *testing.T) {
	contribution, err := local.New("local", t.TempDir(), slog.Default()).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contribution.IndexedCount != 0 || contribution.SkippedCount != 0 || len(contribution.ChartVersions) != 0 {
		t.Fatalf("contribution = %#v", contribution)
	}
}

func writeChart(t *testing.T, filename, name, version, payload string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	metadata := "name: " + name + "\nversion: " + version + "\n"
	for _, entry := range []struct{ name, body string }{{name + "/Chart.yaml", metadata}, {name + "/values.yaml", payload}} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tarWriter, strings.NewReader(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filename, modified, modified); err != nil {
		t.Fatal(err)
	}
}
