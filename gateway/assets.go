package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func validateEmbeddedRelease() error {
	if len(embeddedBootstrapCredential) == 0 {
		return errors.New("embedded bootstrap credential unavailable; use a production build")
	}
	if len(embeddedFFmpeg) == 0 {
		return errors.New("embedded FFmpeg unavailable; use a production build")
	}
	return nil
}

func bootstrapCredentialsJSON() ([]byte, error) {
	if len(embeddedBootstrapCredential) == 0 {
		return nil, errors.New("embedded bootstrap credential unavailable; use a production build")
	}
	return bytes.Clone(embeddedBootstrapCredential), nil
}

// ensureEmbeddedFFmpeg makes FFmpeg a payload of the running Gateway release.
// The installed payload is untouched until its replacement is complete.
func ensureEmbeddedFFmpeg(targetPath string) error {
	if len(embeddedFFmpeg) == 0 {
		return errors.New("embedded FFmpeg unavailable; use a production build")
	}
	expected := sha256.Sum256(embeddedFFmpeg)
	if actual, err := hashFile(targetPath); err == nil && bytes.Equal(actual, expected[:]) {
		return secureInstalledExecutable(targetPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("installed FFmpeg inspection failed: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("FFmpeg directory creation failed: %w", err)
	}
	temporaryPath := targetPath + ".installing"
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("FFmpeg temporary cleanup failed: %w", err)
	}
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("FFmpeg temporary creation failed: %w", err)
	}
	keep := true
	defer func() {
		_ = temporary.Close()
		if keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(embeddedFFmpeg); err != nil {
		return fmt.Errorf("FFmpeg extraction failed: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("FFmpeg sync failed: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("FFmpeg close failed: %w", err)
	}
	if err := secureInstalledExecutable(temporaryPath); err != nil {
		return err
	}
	actual, err := hashFile(temporaryPath)
	if err != nil || !bytes.Equal(actual, expected[:]) {
		return errors.New("extracted FFmpeg verification failed")
	}
	if err := replaceFileAtomically(temporaryPath, targetPath); err != nil {
		return fmt.Errorf("FFmpeg replacement failed: %w", err)
	}
	keep = false
	if err := syncParentDirectory(targetPath); err != nil {
		return fmt.Errorf("FFmpeg directory sync failed: %w", err)
	}
	return nil
}

func hashFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}
