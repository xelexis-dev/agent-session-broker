package controlpanel

// Run every control-state upgrade before serving. Each step is idempotent;
// control.json import and existing SQLite databases use the same pipeline.
func (s *Store) migrateControlState() error {
	if s.data.Version == 1 {
		if err := s.migrate(); err != nil {
			return err
		}
	}
	for _, step := range []func() error{s.migrateSpawnScopes, s.migrateTokenOnly, s.migrateHTTPPolicy, s.backfillTokenHistory, s.migrateRevokedHistory, s.migrateScopes, s.migrateProjectGenerations} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}
