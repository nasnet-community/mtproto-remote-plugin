package telemt

import "testing"

const sample = `# HELP telemt_user_octets_from_client bytes
# TYPE telemt_user_octets_from_client counter
telemt_user_octets_from_client{user="alice@example.com"} 1500
telemt_user_octets_from_client{user="bob",mode="tls"} 20 1727800000
telemt_user_octets_from_client{user="bob",mode="classic"} 5
telemt_user_octets_from_client_total{user="carol"} 99
telemt_user_octets_from_client 7
telemt_user_octets_to_client{user="alice@example.com"} 4200
telemt_user_octets_from_client{user="we}ird\"name"} 3
telemt_user_octets_from_client{user="neg"} -4
`

func TestParseMetricSumsPerUserAndIgnoresLookalikes(t *testing.T) {
	up := ParseMetric(sample, MetricUp)
	want := map[string]uint64{"alice@example.com": 1500, "bob": 25, `we}ird"name`: 3}
	if len(up) != len(want) {
		t.Fatalf("got %v", up)
	}
	for k, v := range want {
		if up[k] != v {
			t.Errorf("%q = %d, want %d", k, up[k], v)
		}
	}
	if down := ParseMetric(sample, MetricDown); down["alice@example.com"] != 4200 || len(down) != 1 {
		t.Errorf("down = %v", down)
	}
}

func TestParseMetricSurvivesGarbage(t *testing.T) {
	for _, in := range []string{"", "\x00\x01", "telemt_user_octets_from_client{", `telemt_user_octets_from_client{user="x"`, `telemt_user_octets_from_client{user=x} 1`} {
		if got := ParseMetric(in, MetricUp); len(got) != 0 {
			t.Errorf("%q parsed to %v", in, got)
		}
	}
}
