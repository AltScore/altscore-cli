package cmd

import "testing"

// An absent dataAge is not an oversight, it is the instruction "use the freshness
// this source publishes". apply used to stamp 30 onto every entry that lacked one,
// which made that instruction unsayable: an authored dataAge overrides the
// published policy, so every applied workflow asked for a half-hourly refresh of
// data its source says is good for anywhere from a day to a fortnight.
func TestAltdataSourceDefaults_DoesNotStampDataAge(t *testing.T) {
	sources := []any{
		map[string]any{"sourceId": "ECU-PUB-0063", "version": "v2"},
	}

	applyAltdataSourceDefaults(sources)

	sm := sources[0].(map[string]any)
	if v, has := sm["dataAge"]; has {
		t.Errorf("dataAge was stamped as %v; an absent dataAge means "+
			"'use the source's published cacheMaxSeconds' and must survive apply", v)
	}
}

func TestAltdataSourceDefaults_KeepsAnAuthoredDataAge(t *testing.T) {
	sources := []any{
		map[string]any{"sourceId": "ECU-PUB-0089", "version": "v1", "dataAge": 60},
	}

	applyAltdataSourceDefaults(sources)

	if got := sources[0].(map[string]any)["dataAge"]; got != 60 {
		t.Errorf("dataAge = %v, want 60 untouched", got)
	}
}

func TestAltdataSourceDefaults_DerivesPackageAliasFromSourceId(t *testing.T) {
	sources := []any{
		map[string]any{"sourceId": "ECU-PUB-0063", "version": "v2"},
		map[string]any{"sourceId": "ECU-PUB-0089", "version": "v1", "packageAlias": "mine"},
	}

	applyAltdataSourceDefaults(sources)

	if got := sources[0].(map[string]any)["packageAlias"]; got != "ecu_pub_0063" {
		t.Errorf("packageAlias = %v, want ecu_pub_0063", got)
	}
	if got := sources[1].(map[string]any)["packageAlias"]; got != "mine" {
		t.Errorf("packageAlias = %v, want the authored value kept", got)
	}
}

func TestAltdataSourceDefaults_SkipsEntriesWithNoSourceId(t *testing.T) {
	sources := []any{
		map[string]any{"version": "v1"},
		"not-a-map",
	}

	applyAltdataSourceDefaults(sources)

	if _, has := sources[0].(map[string]any)["packageAlias"]; has {
		t.Error("packageAlias derived from an empty sourceId")
	}
}
