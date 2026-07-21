package osm

import "testing"

func TestTigerCountyZipPattern(t *testing.T) {
	m := tigerCountyZipPattern.FindStringSubmatch("tl_2025_25017_addrfeat.zip")
	if m == nil || m[1] != "25017" {
		t.Errorf("expected to extract FIPS 25017, got %v", m)
	}

	for _, name := range []string{
		"tl_2025_us_county.zip",
		"tl_2025_25017_addrfeat.zip.part",
		"25017",
		"tl_2025_25017_addrfeat.shp",
	} {
		if tigerCountyZipPattern.MatchString(name) {
			t.Errorf("pattern should not match %q", name)
		}
	}
}

func TestIsDigits(t *testing.T) {
	if !isDigits("25017") {
		t.Error("isDigits(25017) = false, want true")
	}
	if isDigits("county_ref") || isDigits("") {
		t.Error("isDigits should reject non-numeric and empty strings")
	}
}
