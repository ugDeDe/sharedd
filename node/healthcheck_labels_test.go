package main

import "testing"

func TestParsePrometheusSamplesLabelEdgeCases(t *testing.T) {
	text := `
# HELP telemt_user_unique_ips_current help
telemt_me_writers_active_current 3
telemt_user_unique_ips_current{user="u1"} 7
telemt_user_unique_ips_current{user="weird}path",x="1"} 2
telemt_user_unique_ips_current{path="/a\"b, c",user="u2"} 4
broken{labels
`
	samples := parsePrometheusSamples(text)
	byName := map[string][]promSample{}
	for _, s := range samples {
		byName[s.name] = append(byName[s.name], s)
	}

	if len(byName["telemt_me_writers_active_current"]) != 1 {
		t.Fatalf("plain series lost: %+v", byName)
	}
	users := map[string]string{}
	for _, s := range byName["telemt_user_unique_ips_current"] {
		users[promUserLabel(s.labels)] = s.labels
	}
	if _, ok := users["u1"]; !ok {
		t.Fatalf("simple user label lost: %+v", users)
	}
	if _, ok := users["weird}path"]; !ok {
		t.Fatalf("'}' inside quoted label value broke parsing: %+v", users)
	}
	if _, ok := users["u2"]; !ok {
		t.Fatalf("escaped quote/comma in labels broke parsing: %+v", users)
	}
	if _, ok := byName["broken"]; ok {
		t.Fatal("unterminated labels must be skipped")
	}
}
