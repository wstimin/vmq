package maintenance

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

var Version = "v3.1.0-web.1"
var DefaultRepository = "wstimin/vmq"

func hexSecret(value string) ([]byte, error) { return hex.DecodeString(value) }

func writeNewPrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".vmq-new-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Link(f.Name(), path)
}

func randomSecret() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".vmq-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
