package models

// Place represents a geocoded address record.
type Place struct {
	ID         string  `db:"id" json:"id"`
	OSMID      int64   `db:"osm_id" json:"osm_id"`
	OSMType    string  `db:"osm_type" json:"osm_type"`
	Name       string  `db:"name" json:"name"`
	Address    string  `db:"address" json:"address"`
	City       string  `db:"city" json:"city"`
	State      string  `db:"state" json:"state"`
	Postcode   string  `db:"postcode" json:"postcode"`
	Country    string  `db:"country" json:"country"`
	Lat        float64 `db:"lat" json:"lat"`
	Lon        float64 `db:"lon" json:"lon"`
	Class      string  `db:"class" json:"class"`
	Type       string  `db:"type" json:"type"`
	Importance float64 `db:"importance" json:"importance"`
	Created    string  `db:"created" json:"created"`
	Updated    string  `db:"updated" json:"updated"`
}
