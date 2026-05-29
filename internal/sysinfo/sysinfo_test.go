package sysinfo

import "testing"

const sampleVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                5178.
Pages active:                            558482.
Pages inactive:                          560478.
Pages speculative:                         1999.
Pages throttled:                              0.
Pages wired down:                        171372.
Pages purgeable:                           2721.
Pages occupied by compressor:             40000.
`

func TestParseVMStat(t *testing.T) {
	ps, counts := parseVMStat(sampleVMStat)
	if ps != 16384 {
		t.Fatalf("page size = %d, want 16384", ps)
	}
	if counts["free"] != 5178 {
		t.Errorf("free = %d, want 5178", counts["free"])
	}
	if counts["wired down"] != 171372 {
		t.Errorf("wired down = %d, want 171372", counts["wired down"])
	}
	if counts["occupied by compressor"] != 40000 {
		t.Errorf("compressor = %d, want 40000", counts["occupied by compressor"])
	}
}

func TestParseSwapUsed(t *testing.T) {
	cases := map[string]uint64{
		"total = 2048.00M  used = 512.00M  free = 1536.00M  (encrypted)": 512 << 20,
		"total = 0.00M  used = 0.00M  free = 0.00M":                      0,
		"total = 4.00G  used = 1.50G  free = 2.50G":                      uint64(1.5 * (1 << 30)),
	}
	for in, want := range cases {
		if got := parseSwapUsed(in); got != want {
			t.Errorf("parseSwapUsed(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseHumanBytes(t *testing.T) {
	cases := map[string]uint64{
		"512.00M": 512 << 20,
		"1.50G":   uint64(1.5 * (1 << 30)),
		"100K":    100 << 10,
		"":        0,
		"garbage": 0,
	}
	for in, want := range cases {
		if got := parseHumanBytes(in); got != want {
			t.Errorf("parseHumanBytes(%q) = %d, want %d", in, got, want)
		}
	}
}
