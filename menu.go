package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// menuSnap is an immutable, ready-to-serve copy of the whole menu.
// Thousands of POS screens read it per second without touching the database;
// it rebuilds itself only after someone edits menu / recipes / taxes / costs.
type MChoice struct {
	Name  string `json:"name"`
	Price int64  `json:"price"`
}

type MOption struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Required bool      `json:"required"`
	Choices  []MChoice `json:"choices"`
}

type MProduct struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Category  int64   `json:"category"`
	CatName   string  `json:"cat_name,omitempty"`
	Price     int64   `json:"price"`
	TaxRate   float64 `json:"tax_rate"`
	Cost      int64   `json:"-"`
	Station   string  `json:"station,omitempty"`
	Emoji     string  `json:"emoji,omitempty"`
	Image     string  `json:"image,omitempty"`
	Desc      string  `json:"desc,omitempty"`
	Options   []int64 `json:"options,omitempty"`
	Available bool    `json:"available"`
	Prep      int     `json:"prep"`
	Featured  bool    `json:"featured,omitempty"`
	SKU       string  `json:"sku,omitempty"`
	Barcode   string  `json:"barcode,omitempty"`
	Track     bool    `json:"-"`
}

type recipeLine struct {
	Ingredient int64
	Qty        float64 // per single portion sold
}

type menuSnap struct {
	ver      int64
	json     []byte
	etag     string
	products map[int64]*MProduct
	options  map[int64]*MOption
	recipes  map[int64][]recipeLine
}

var menuVerKeys = []string{"products", "categories", "options", "stations", "recipes", "taxes", "ingredients", "menus"}

func menuVersion() int64 {
	var v int64
	for _, k := range menuVerKeys {
		v += verOf(k) * 7
	}
	return v
}

// menuDirty is called by edit hooks; the snapshot actually rebuilds from
// resource version counters that bump *after* the transaction commits.
func (a *App) menuDirty() {}

var menuMu sync.Mutex

func (a *App) menu() *menuSnap {
	v := menuVersion()
	if m := a.menuSnap.Load(); m != nil && m.ver == v {
		return m
	}
	menuMu.Lock()
	defer menuMu.Unlock()
	if m := a.menuSnap.Load(); m != nil && m.ver == v {
		return m
	}
	m := a.buildMenu(v)
	a.menuSnap.Store(m)
	return m
}

func (a *App) buildMenu(ver int64) *menuSnap {
	q := a.db.rdb
	snap := &menuSnap{ver: ver, products: map[int64]*MProduct{}, options: map[int64]*MOption{}, recipes: map[int64][]recipeLine{}}

	stations := map[int64]string{}
	for _, r := range recs(q, "stations", 0) {
		stations[r.ID] = r.S("name")
	}
	type cat struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Emoji   string `json:"emoji,omitempty"`
		Sort    int64  `json:"sort"`
		Station int64  `json:"-"`
	}
	var cats []cat
	catName := map[int64]string{}
	catStation := map[int64]string{}
	for _, r := range recs(q, "categories", 0) {
		if r.Status == "inactive" {
			continue
		}
		cats = append(cats, cat{r.ID, r.S("name"), r.S("emoji"), r.I("sort"), r.I("station")})
		catName[r.ID] = r.S("name")
		catStation[r.ID] = stations[r.I("station")]
	}
	sort.SliceStable(cats, func(i, j int) bool { return cats[i].Sort < cats[j].Sort })

	taxRate := map[int64]float64{}
	var defTax float64
	for _, r := range recs(q, "taxes", 0) {
		if r.Status == "inactive" {
			continue
		}
		taxRate[r.ID] = r.N("rate")
		if r.B("default") {
			defTax = r.N("rate")
		}
	}

	for _, r := range recs(q, "options", 0) {
		if r.Status == "inactive" {
			continue
		}
		o := &MOption{ID: r.ID, Name: r.S("name"), Kind: r.S("kind"), Required: r.B("required")}
		for _, c := range r.L("choices") {
			o.Choices = append(o.Choices, MChoice{toStr(c["name"]), toInt(c["price"])})
		}
		snap.options[r.ID] = o
	}

	// recipe cost per portion
	recipeCost := map[int64]int64{}
	for _, r := range recs(q, "recipes", 0) {
		pid := r.I("product")
		portions := r.N("portions")
		if portions <= 0 {
			portions = 1
		}
		waste := 1 + r.N("waste_pct")/100
		var cost float64
		var lines []recipeLine
		for _, l := range r.L("lines") {
			per := toFloat(l["qty"]) / portions * waste
			ing := toInt(l["ingredient"])
			lines = append(lines, recipeLine{ing, per})
			cost += per * float64(a.ing(q, ing).Cost)
		}
		snap.recipes[pid] = lines
		recipeCost[pid] = int64(cost)
	}

	var plist []*MProduct
	for _, r := range recs(q, "products", 0) {
		if r.Status == "inactive" {
			continue
		}
		p := &MProduct{ID: r.ID, Name: r.S("name"), Category: r.I("category"), CatName: catName[r.I("category")], Price: r.I("price"),
			Emoji: r.S("emoji"), Image: r.S("image"), Desc: r.S("description"), Available: r.B("available"), Prep: int(r.I("prep_min")),
			Featured: r.B("featured"), SKU: r.Ref, Barcode: r.S("barcode"), Station: catStation[r.I("category")], Track: r.B("track_stock")}
		if t := r.I("tax"); t > 0 {
			p.TaxRate = taxRate[t]
		} else {
			p.TaxRate = defTax
		}
		p.Cost = r.I("cost")
		if c, ok := recipeCost[r.ID]; ok {
			p.Cost = c
		}
		p.Options = r.IDs("options")
		snap.products[r.ID] = p
		plist = append(plist, p)
	}
	sort.SliceStable(plist, func(i, j int) bool { return plist[i].Name < plist[j].Name })

	opts := make([]*MOption, 0, len(snap.options))
	for _, o := range snap.options {
		opts = append(opts, o)
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].ID < opts[j].ID })

	b, _ := json.Marshal(map[string]any{"categories": cats, "products": plist, "options": opts})
	sum := sha1.Sum(b)
	snap.json = b
	snap.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	return snap
}

func (a *App) handleMenu(w http.ResponseWriter, r *http.Request) {
	m := a.menu()
	w.Header().Set("ETag", m.etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == m.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(m.json)
}
