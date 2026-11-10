package clientstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type deliveryWatermark struct {
	RegistrationID string `json:"registrationId"`
	Epoch          string `json:"epoch"`
	Through        uint64 `json:"through"`
}

func (s *Store) deliveryPath(scope, registration, epoch string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + registration + "\x00" + epoch))
	return filepath.Join(s.dir, "delivery", hex.EncodeToString(sum[:16])+".json")
}
func (s *Store) LoadDeliveryWatermark(scope, registration, epoch string) (uint64, error) {
	file, err := os.Open(s.deliveryPath(scope, registration, epoch))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, errors.New("delivery watermark is unreadable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return 0, errors.New("delivery watermark is unreadable")
	}
	var value deliveryWatermark
	if len(data) > 1024 || json.Unmarshal(data, &value) != nil || value.RegistrationID != registration || value.Epoch != epoch {
		return 0, errors.New("delivery watermark is invalid")
	}
	return value.Through, nil
}

// Serialize read/replace across client processes; metadata never contains a payload.
func (s *Store) SaveDeliveryWatermark(scope, registration, epoch string, through uint64) error {
	if registration == "" || epoch == "" {
		return errors.New("delivery watermark requires registration and epoch")
	}
	path := s.deliveryPath(scope, registration, epoch)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("delivery state is unavailable")
	}
	unlock, err := lockFileWait(path+".lock", time.Second)
	if err != nil {
		return errors.New("delivery state is busy")
	}
	defer unlock()
	prior, err := s.LoadDeliveryWatermark(scope, registration, epoch)
	if err != nil {
		return err
	}
	if prior >= through {
		return nil
	}
	data, err := json.Marshal(deliveryWatermark{RegistrationID: registration, Epoch: epoch, Through: through})
	if err != nil {
		return err
	}
	if writeFile(path, data) != nil {
		return errors.New("delivery watermark could not be saved")
	}
	return nil
}

// DeleteDeliveryWatermark removes only this proven-obsolete epoch. Busy writers
// are skipped, and the lock inode stays available to current and older clients.
func (s *Store) DeleteDeliveryWatermark(scope, registration, epoch string) error {
	if registration == "" || epoch == "" {
		return errors.New("delivery watermark requires registration and epoch")
	}
	path := s.deliveryPath(scope, registration, epoch)
	if _, ok := plainPath(s.dir, true); !ok {
		return nil
	}
	if _, ok := plainPath(filepath.Dir(path), true); !ok {
		return nil
	}
	before, ok := plainPath(path, false)
	if !ok || !safeLockPath(path+".lock") {
		return nil
	}
	unlock, err := lockFile(path + ".lock")
	if errors.Is(err, ErrLocked) {
		return nil
	}
	if err != nil {
		return errors.New("delivery state is unavailable")
	}
	defer unlock()
	current, ok := plainPath(path, false)
	if !ok || !os.SameFile(before, current) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.New("delivery watermark could not be removed")
	}
	return nil
}
