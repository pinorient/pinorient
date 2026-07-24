package osm

import (
	"testing"

	"github.com/qedus/osmpbf"
)

func TestClassType(t *testing.T) {
	cases := []struct {
		tags      map[string]string
		wantClass string
		wantType  string
	}{
		{map[string]string{"amenity": "university"}, "amenity", "university"},
		{map[string]string{"building": "college"}, "building", "college"},
		// amenity outranks building in the priority list.
		{map[string]string{"building": "yes", "amenity": "hospital"}, "amenity", "hospital"},
		{map[string]string{"place": "city", "name": "Portland"}, "place", "city"},
		{map[string]string{"highway": "residential"}, "highway", "residential"},
		{map[string]string{"tourism": "museum"}, "tourism", "museum"},
		// No recognized class keys.
		{map[string]string{"name": "Only A Name"}, "", ""},
		{map[string]string{}, "", ""},
	}
	for _, c := range cases {
		class, typ := classType(c.tags)
		if class != c.wantClass || typ != c.wantType {
			t.Errorf("classType(%v) = (%q, %q), want (%q, %q)", c.tags, class, typ, c.wantClass, c.wantType)
		}
	}
}

func TestPlaceImportance(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want float64
	}{
		{"city", map[string]string{"place": "city", "name": "Portland"}, 0.45},
		{"town", map[string]string{"place": "town", "name": "Brunswick"}, 0.40},
		{"neighbourhood", map[string]string{"place": "neighbourhood", "name": "West End"}, 0.28},
		{"named POI", map[string]string{"name": "Bowdoin College", "amenity": "university"}, 0.15},
		{"named road", map[string]string{"name": "Main Street", "highway": "residential"}, 0.10},
		{"named building", map[string]string{"name": "Roux Center", "building": "college"}, 0.05},
		{"unnamed address", map[string]string{"addr:housenumber": "15", "addr:street": "Maple St"}, 0},
	}
	for _, c := range cases {
		if got := placeImportance(c.tags, classType0(c.tags)); got != c.want {
			t.Errorf("%s: placeImportance(%v) = %v, want %v", c.name, c.tags, got, c.want)
		}
	}
}

// classType0 is a test helper returning just the class for importance checks.
func classType0(tags map[string]string) string {
	class, _ := classType(tags)
	return class
}

func TestNodeToPlaceDerivesClassType(t *testing.T) {
	n := &osmpbf.Node{
		ID: 6053453526, Lat: 43.9104551, Lon: -69.9627045,
		Tags: map[string]string{"name": "Bowdoin College", "amenity": "university"},
	}
	p := nodeToPlace(n)
	if p == nil {
		t.Fatal("nodeToPlace returned nil for a named amenity node")
	}
	if p.Class != "amenity" || p.Type != "university" {
		t.Errorf("class/type = (%q, %q), want (\"amenity\", \"university\")", p.Class, p.Type)
	}
	if p.Importance != 0.15 {
		t.Errorf("importance = %v, want 0.15", p.Importance)
	}
	if p.Lat != 43.9104551 || p.Lon != -69.9627045 {
		t.Errorf("coords = (%v, %v), want (43.9104551, -69.9627045)", p.Lat, p.Lon)
	}
}

func TestWayToPlaceGates(t *testing.T) {
	// Named building with address tags: indexed, class derived from tags.
	w := &osmpbf.Way{
		ID: 770655187,
		Tags: map[string]string{
			"name": "Roux Center for the Environment", "building": "college",
			"addr:housenumber": "44", "addr:street": "College Street",
			"addr:city": "Brunswick", "addr:state": "ME", "addr:postcode": "04011",
		},
	}
	p := wayToPlace(w)
	if p == nil {
		t.Fatal("wayToPlace returned nil for a named building way")
	}
	if p.Class != "building" || p.Type != "college" {
		t.Errorf("class/type = (%q, %q), want (\"building\", \"college\")", p.Class, p.Type)
	}
	if p.Address != "44 College Street" {
		t.Errorf("address = %q, want \"44 College Street\"", p.Address)
	}
	if p.City != "Brunswick" || p.State != "ME" || p.Postcode != "04011" {
		t.Errorf("city/state/postcode = (%q, %q, %q)", p.City, p.State, p.Postcode)
	}

	// House number + street without a name: name is synthesized.
	w2 := &osmpbf.Way{ID: 2, Tags: map[string]string{"addr:housenumber": "42", "addr:street": "Maple Street"}}
	p2 := wayToPlace(w2)
	if p2 == nil || p2.Name != "42 Maple Street" {
		t.Errorf("wayToPlace(addr way) = %+v, want name \"42 Maple Street\"", p2)
	}

	// addr:city only: nothing to build a searchable name from — not indexed.
	w3 := &osmpbf.Way{ID: 3, Tags: map[string]string{"addr:city": "Nowhere"}}
	if wayToPlace(w3) != nil {
		t.Error("wayToPlace should reject a way with only addr:city")
	}

	// No relevant tags at all.
	if wayToPlace(&osmpbf.Way{ID: 4, Tags: map[string]string{"building": "yes"}}) != nil {
		t.Error("wayToPlace should reject an unnamed, unaddressed way")
	}
}

func TestIsIndexedWay(t *testing.T) {
	cases := []struct {
		tags map[string]string
		want bool
	}{
		{map[string]string{"name": "Roux Center"}, true},
		{map[string]string{"addr:housenumber": "15"}, true},
		{map[string]string{"addr:street": "Maple Street"}, true},
		{map[string]string{"addr:city": "Nowhere"}, false},
		{map[string]string{"building": "yes"}, false},
		{map[string]string{}, false},
	}
	for _, c := range cases {
		w := &osmpbf.Way{ID: 1, Tags: c.tags}
		if got := isIndexedWay(w); got != c.want {
			t.Errorf("isIndexedWay(%v) = %v, want %v", c.tags, got, c.want)
		}
	}
}
