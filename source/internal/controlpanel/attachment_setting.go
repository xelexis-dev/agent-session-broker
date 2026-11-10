package controlpanel

import (
	"bytes"
	"encoding/json"
	"github.com/xelexis-dev/agent-session-broker/source/internal/attachments"
	"io"
	"net/http"
	"time"
)

const attachmentSettingKey = "settings/attachments"

type attachmentSetting struct {
	Enabled *policyBool `json:"enabled"`
	Actor   string      `json:"actor"`
	At      time.Time   `json:"at"`
}

func decodeAttachmentSetting(raw []byte) *attachmentSetting {
	if len(raw) > 4096 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	var p attachmentSetting
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil
		}
		k, ok := key.(string)
		if !ok || seen[k] {
			return nil
		}
		seen[k] = true
		switch k {
		case "enabled":
			if d.Decode(&p.Enabled) != nil {
				return nil
			}
		case "actor":
			if d.Decode(&p.Actor) != nil {
				return nil
			}
		case "at":
			if d.Decode(&p.At) != nil {
				return nil
			}
		default:
			return nil
		}
	}
	if _, err = d.Token(); err != nil {
		return nil
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil
	}
	if len(seen) != 3 || p.Enabled == nil || p.Actor == "" || len(p.Actor) > 128 || p.At.IsZero() {
		return nil
	}
	return &p
}
func (s *Store) AttachmentsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && s.attachmentEnabled
}
func (s *Store) attachmentPolicySnapshot() (bool, string, *attachmentSetting) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	enabled := !s.closed && s.attachmentEnabled
	version := policySettingsVersion("attachments", enabled)
	if s.attachmentLastChange == nil {
		return enabled, version, nil
	}
	last := *s.attachmentLastChange
	value := *last.Enabled
	last.Enabled = &value
	return enabled, version, &last
}
func (s *Store) SetAttachmentStore(store *attachments.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachmentStore = store
	s.attachmentDiskCapacity = store.DiskCapacity
	store.SetEnabled(!s.closed && s.attachmentEnabled)
}
func (s *Store) SetAttachmentPolicy(actor string, enabled bool, version ...string) error {
	return s.setAttachmentPolicyGuarded(actor, enabled, nil, version...)
}
func (s *Store) setAttachmentPolicyGuarded(actor string, enabled bool, guard commitGuard, version ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := guard.check(); err != nil {
		return err
	}
	if s.closed || s.data.Admin == nil || s.writeBatch == nil {
		return ErrStorage
	}
	if !s.isAdminLocked(actor) {
		return ErrForbidden
	}
	if err := checkSettingsVersion(version, policySettingsVersion("attachments", s.attachmentEnabled)); err != nil {
		return err
	}
	if s.attachmentEnabled == enabled {
		return nil
	}
	value := policyBool(enabled)
	change := &attachmentSetting{&value, actor, s.now().UTC()}
	data, err := json.Marshal(change)
	if err != nil {
		return ErrStorage
	}
	if s.writeBatch(map[string][]byte{attachmentSettingKey: data}, nil) != nil {
		return ErrStorage
	}
	s.attachmentLastChange = change
	s.attachmentEnabled = enabled
	if s.attachmentStore != nil {
		s.attachmentStore.SetEnabled(enabled)
	}
	return nil
}
func (p *panel) attachmentPolicyMutate(w http.ResponseWriter, r *http.Request, session loginSession) {
	user, ok := p.store.User(session.userID)
	if !ok || !user.Enabled || user.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	for k, v := range r.PostForm {
		if k == "settings_version" {
			continue
		}
		if (k != "csrf" && k != "attachments") || len(v) != 1 {
			http.Error(w, "invalid form", 400)
			return
		}
	}
	enabled := false
	if v, ok := r.PostForm["attachments"]; ok {
		if v[0] != "true" && v[0] != "false" {
			http.Error(w, "invalid form", 400)
			return
		}
		enabled = v[0] == "true"
	}
	if len(r.PostForm["settings_version"]) != 1 || r.PostForm["settings_version"][0] != policySettingsVersion("attachments", p.store.AttachmentsEnabled()) {
		p.show(w, r, session, "admin-settings", "", 409, settingsConflictMessage, "")
		return
	}
	if err := p.store.SetAttachmentPolicy(session.userID, enabled, r.PostForm["settings_version"]...); err != nil {
		http.Error(w, "attachment policy refused", 403)
		return
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}
