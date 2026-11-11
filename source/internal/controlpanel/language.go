package controlpanel

import (
	"encoding/json"
	"net/http"
	"strings"
)

const languageCookie = "asb_lang"

const userSettingsPrefix = "user-settings/"

type userSettings struct {
	V        int    `json:"v"`
	Language string `json:"language"`
}

func validLanguage(language string) bool { return language == "en" || language == "ko" }

// apiLanguage distinguishes an omitted field from invalid null/empty values.
type apiLanguage string

func (v *apiLanguage) UnmarshalJSON(raw []byte) error {
	var language string
	if json.Unmarshal(raw, &language) != nil || !validLanguage(language) {
		return ErrInvalid
	}
	*v = apiLanguage(language)
	return nil
}

func (s *Store) loadLanguages(records map[string][]byte) {
	s.languages = map[string]string{}
	s.invalidUserSettings = 0
	for key, raw := range records {
		if !strings.HasPrefix(key, userSettingsPrefix) {
			continue
		}
		id := strings.TrimPrefix(key, userSettingsPrefix)
		var settings userSettings
		if len(raw) > 1024 || !usernamePattern.MatchString(id) || json.Unmarshal(raw, &settings) != nil || settings.V != 1 || !validLanguage(settings.Language) {
			s.invalidUserSettings++
			continue
		}
		s.languages[id] = settings.Language
	}
}

func (s *Store) languageLocked(id string) string {
	if language := s.languages[id]; validLanguage(language) {
		return language
	}
	return "en"
}

// UserLanguage returns an account preference without adding it to the persisted User.
func (s *Store) UserLanguage(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.languageLocked(id)
}

func (s *Store) UpdateLanguage(actor, language string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.activeUserLocked(actor); !ok {
		return ErrForbidden
	}
	if !validLanguage(language) {
		return ErrInvalid
	}
	raw, _ := json.Marshal(userSettings{V: 1, Language: language})
	if s.writeBatch == nil || s.writeBatch(map[string][]byte{userSettingsPrefix + actor: raw}, nil) != nil {
		return ErrStorage
	}
	if s.languages == nil {
		s.languages = map[string]string{}
	}
	s.languages[actor] = language
	return nil
}

func (p *panel) setLanguageCookie(w http.ResponseWriter, r *http.Request, language string) {
	http.SetCookie(w, &http.Cookie{Name: languageCookie, Value: language, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: p.secure(r), SameSite: http.SameSiteLaxMode})
}
