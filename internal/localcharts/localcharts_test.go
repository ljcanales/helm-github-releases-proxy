package localcharts

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanReadsValidChartsAndSkipsBadFiles(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "demo-chart-v1.2.3.tgz")
	writeChart(t, valid, "demo-chart", "1.2.3")
	if err := os.WriteFile(filepath.Join(directory, "not-a-chart.tgz"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	charts, skipped, err := Scan(directory, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(charts) != 1 || skipped != 1 {
		t.Fatalf("charts = %#v, skipped = %d", charts, skipped)
	}
	if charts[0].Name != "demo-chart" || charts[0].Version != "1.2.3" || charts[0].Filename != "demo-chart-v1.2.3.tgz" {
		t.Fatalf("chart = %#v", charts[0])
	}
	data, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if charts[0].Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %q", charts[0].Digest)
	}
}

func TestScanRequiresMatchingChartMetadata(t *testing.T) {
	directory := t.TempDir()
	writeChart(t, filepath.Join(directory, "demo-1.2.3.tgz"), "other", "1.2.3")
	writeChart(t, filepath.Join(directory, "demo-1.2.4.tgz"), "demo", "v1.2.3")

	charts, skipped, err := Scan(directory, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(charts) != 0 || skipped != 2 {
		t.Fatalf("charts = %#v, skipped = %d", charts, skipped)
	}
}

func writeChart(t *testing.T, filename, name, version string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	metadata := "name: " + name + "\nversion: " + version + "\n"
	if err := tarWriter.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(metadata))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(tarWriter, strings.NewReader(metadata)); err != nil {
		t.Fatal(err)
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
	if err := os.Chtimes(filename, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), time.Time{}); err != nil {
		t.Fatal(err)
	}
}
