package agent

import (
	"os"
	"path/filepath"
	"testing"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestValidateImagesRejectsMismatchedContent(t *testing.T) {
	err := ValidateImages([]Image{{
		Name:      "not-an-image.png",
		MediaType: "image/png",
		Data:      "aGVsbG8=",
	}})
	if err == nil {
		t.Fatal("expected invalid image error")
	}
}

func TestAttachmentStoreWritesAndRemovesPrivateImage(t *testing.T) {
	store, err := NewAttachmentStore()
	if err != nil {
		t.Fatal(err)
	}
	directory := store.Directory()
	defer store.Close()

	paths, err := store.Write([]Image{{
		Name:      "pixel.png",
		MediaType: "image/png",
		Data:      onePixelPNG,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Dir(paths[0]) != directory {
		t.Fatalf("unexpected attachment paths: %#v", paths)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal(err)
	}
	store.Remove(paths)
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("attachment still exists: %v", err)
	}
}
