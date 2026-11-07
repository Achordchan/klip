package model

import "encoding/json"

// StoreImagesAsWebP defaults on for existing installations. Extra preserves
// the legacy C# settings schema and older readers retain the unknown property.
func (s Settings) StoreImagesAsWebP() bool {
	var enabled bool
	raw, ok := s.Extra["StoreImagesAsWebP"]
	if !ok || json.Unmarshal(raw, &enabled) != nil {
		return true
	}
	return enabled
}
func (s *Settings) SetStoreImagesAsWebP(enabled bool) {
	extra := make(map[string]json.RawMessage, len(s.Extra)+1)
	for k, v := range s.Extra {
		extra[k] = v
	}
	extra["StoreImagesAsWebP"], _ = json.Marshal(enabled)
	s.Extra = extra
}
