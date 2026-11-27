package render

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendPNGChunk(dst *bytes.Buffer, typ string, data []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	dst.Write(lenBuf[:])
	dst.WriteString(typ)
	dst.Write(data)
	h := crc32.NewIEEE()
	_, _ = h.Write([]byte(typ))
	_, _ = h.Write(data)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], h.Sum32())
	dst.Write(crcBuf[:])
}

func testPaethPredictor(a, b, c uint8) uint8 {
	pa := int(b) - int(c)
	pb := int(a) - int(c)
	pc := pa + pb
	if pa < 0 {
		pa = -pa
	}
	if pb < 0 {
		pb = -pb
	}
	if pc < 0 {
		pc = -pc
	}
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

// buildCustomPNG encodes raw scanlines using the chosen PNG filter type (0..4) and colorType
// (0=Gray, 2=RGB, 3=Palette, 4=GrayA, 6=RGBA), with optional PLTE and tRNS chunks.
func buildCustomPNG(t *testing.T, w, h int, colorType, filterType byte, rawRows [][]byte, plte, trns []byte) []byte {
	t.Helper()
	bpp, err := pngBytesPerPixel(colorType)
	if err != nil {
		t.Fatal(err)
	}
	var rawStream bytes.Buffer
	prev := make([]byte, w*bpp)
	filtered := make([]byte, w*bpp)

	for y := range h {
		cur := rawRows[y]
		rawStream.WriteByte(filterType)
		for i := range cur {
			var a, b, c uint8
			if i >= bpp {
				a = cur[i-bpp]
				c = prev[i-bpp]
			}
			b = prev[i]
			switch filterType {
			case 0:
				filtered[i] = cur[i]
			case 1:
				filtered[i] = cur[i] - a
			case 2:
				filtered[i] = cur[i] - b
			case 3:
				filtered[i] = cur[i] - uint8((int(a)+int(b))>>1)
			case 4:
				filtered[i] = cur[i] - testPaethPredictor(a, b, c)
			default:
				t.Fatalf("unsupported test filter %d", filterType)
			}
		}
		rawStream.Write(filtered)
		copy(prev, cur)
	}

	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	if _, err := zw.Write(rawStream.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	out.WriteString(pngSignature)

	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8
	ihdr[9] = colorType
	appendPNGChunk(&out, "IHDR", ihdr[:])
	if len(plte) > 0 {
		appendPNGChunk(&out, "PLTE", plte)
	}
	if len(trns) > 0 {
		appendPNGChunk(&out, "tRNS", trns)
	}
	// Split IDAT across two chunks to also exercise multi-IDAT streaming.
	idatBytes := idat.Bytes()
	mid := len(idatBytes) / 2
	appendPNGChunk(&out, "IDAT", idatBytes[:mid])
	appendPNGChunk(&out, "IDAT", idatBytes[mid:])
	appendPNGChunk(&out, "IEND", nil)
	return out.Bytes()
}

func TestStreamingPNGAllFiltersAndColorTypes(t *testing.T) {
	const w, h = 640, 420
	img := buildTestDiagramImage(w, h)
	rgbRows := make([][]byte, h)
	rgbaRows := make([][]byte, h)
	for y := range h {
		rgb := make([]byte, w*3)
		rgba := make([]byte, w*4)
		for x := range w {
			c := img.NRGBAAt(x, y)
			rgb[x*3], rgb[x*3+1], rgb[x*3+2] = c.R, c.G, c.B
			rgba[x*4], rgba[x*4+1], rgba[x*4+2], rgba[x*4+3] = c.R, c.G, c.B, c.A
		}
		rgbRows[y] = rgb
		rgbaRows[y] = rgba
	}

	// Test all 5 PNG scanline filters on RGB (colorType=2) and RGBA (colorType=6).
	for _, ct := range []struct {
		name string
		code byte
		rows [][]byte
	}{
		{name: "RGB", code: 2, rows: rgbRows},
		{name: "RGBA", code: 6, rows: rgbaRows},
	} {
		for filter := byte(0); filter <= 4; filter++ {
			t.Run(ct.name+"_filter_"+string(rune('0'+filter)), func(t *testing.T) {
				raw := buildCustomPNG(t, w, h, ct.code, filter, ct.rows, nil, nil)
				vr := &ViewReport{
					ID:        "v_ok",
					NodeBoxes: []Box{{ID: "box_ok", X: 28, Y: 20, Width: 80, Height: 80}},
					Status:    "PASS",
				}
				gotW, gotH, issues := InspectPNGBytes(raw, vr)
				if gotW != w || gotH != h || len(issues) != 0 {
					t.Fatalf("InspectPNGBytes(%s filter=%d) = (%dx%d, %v), want (%dx%d, nil)", ct.name, filter, gotW, gotH, issues, w, h)
				}
			})
		}
	}

	// Indexed Palette (colorType=3) with PLTE + tRNS.
	t.Run("Palette_PLTE_tRNS", func(t *testing.T) {
		palette := []color.NRGBA{
			{R: 248, G: 250, B: 252, A: 255},
			{R: 15, G: 23, B: 42, A: 255},
			{R: 37, G: 99, B: 235, A: 255},
			{R: 13, G: 148, B: 136, A: 255},
			{R: 99, G: 102, B: 241, A: 255},
			{R: 217, G: 119, B: 6, A: 255},
			{R: 220, G: 38, B: 38, A: 255},
			{R: 5, G: 150, B: 105, A: 255},
			{R: 124, G: 58, B: 237, A: 255},
			{R: 2, G: 132, B: 199, A: 200},
		}
		var plte, trns []byte
		for _, c := range palette {
			plte = append(plte, c.R, c.G, c.B)
			trns = append(trns, c.A)
		}
		rows := make([][]byte, h)
		for y := range h {
			row := make([]byte, w)
			if y >= 40 && y < 140 {
				for i := 1; i < len(palette); i++ {
					x0 := 30 + (i-1)*60
					for x := x0; x < x0+40; x++ {
						row[x] = byte(i)
					}
				}
			}
			rows[y] = row
		}
		raw := buildCustomPNG(t, w, h, 3, 1, rows, plte, trns)
		gotW, gotH, issues := InspectPNGBytes(raw, nil)
		if gotW != w || gotH != h || len(issues) != 0 {
			t.Fatalf("Palette PNG failed: %dx%d issues=%v", gotW, gotH, issues)
		}
	})

	// Grayscale (colorType=0) and Grayscale+Alpha (colorType=4): must decode cleanly and detect monochrome.
	t.Run("Grayscale_and_GrayAlpha", func(t *testing.T) {
		grayRows := make([][]byte, h)
		gaRows := make([][]byte, h)
		for y := range h {
			g := make([]byte, w)
			ga := make([]byte, w*2)
			for x := range w {
				v := byte(250)
				if y >= 40 && y < 240 && x >= 40 && x < 400 {
					v = byte(20 + ((x-40)/36)*20)
				}
				g[x] = v
				ga[x*2] = v
				ga[x*2+1] = 255
			}
			grayRows[y] = g
			gaRows[y] = ga
		}
		for _, tc := range []struct {
			name string
			ct   byte
			rows [][]byte
		}{
			{name: "Gray", ct: 0, rows: grayRows},
			{name: "GrayA", ct: 4, rows: gaRows},
		} {
			raw := buildCustomPNG(t, w, h, tc.ct, 4, tc.rows, nil, nil)
			gotW, gotH, issues := InspectPNGBytes(raw, nil)
			if gotW != w || gotH != h {
				t.Fatalf("%s dimensions = %dx%d, want %dx%d", tc.name, gotW, gotH, w, h)
			}
			if !strings.Contains(strings.Join(issues, ";"), "png_monochrome") {
				t.Fatalf("%s expected png_monochrome issue, got %v", tc.name, issues)
			}
		}
	})

	// 16-bit RGBA fallback via png.Decode.
	t.Run("Fallback16BitNRGBA64", func(t *testing.T) {
		img16 := image.NewNRGBA64(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				c := img.NRGBAAt(x, y)
				img16.SetNRGBA64(x, y, color.NRGBA64{
					R: uint16(c.R) * 0x101,
					G: uint16(c.G) * 0x101,
					B: uint16(c.B) * 0x101,
					A: uint16(c.A) * 0x101,
				})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img16); err != nil {
			t.Fatal(err)
		}
		gotW, gotH, issues := InspectPNGBytes(buf.Bytes(), nil)
		if gotW != w || gotH != h || len(issues) != 0 {
			t.Fatalf("16-bit fallback failed: %dx%d issues=%v", gotW, gotH, issues)
		}
	})
}

func TestStreamingPNGTwoPassBorderFrameAndEdgeCases(t *testing.T) {
	// 1px dark outer border triggers 2nd streaming pass (row 0 is border, dominant BG is white) and passes cleanly.
	t.Run("OnePxDarkBorderPassesTwoPass", func(t *testing.T) {
		img := buildTestDiagramImage(800, 600)
		border := color.NRGBA{R: 30, G: 36, B: 44, A: 255}
		for x := range 800 {
			img.SetNRGBA(x, 0, border)
			img.SetNRGBA(x, 599, border)
		}
		for y := range 600 {
			img.SetNRGBA(0, y, border)
			img.SetNRGBA(799, y, border)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if w, h, issues := InspectPNGBytes(buf.Bytes(), nil); w != 800 || h != 600 || len(issues) != 0 {
			t.Fatalf("1px dark outer border failed: %dx%d issues=%v", w, h, issues)
		}
	})

	// Clipped edges detected on top, bottom, left, right.
	for _, edge := range []string{"top", "bottom", "left", "right"} {
		t.Run("Clipped_"+edge, func(t *testing.T) {
			img := buildTestDiagramImage(800, 600)
			blue := color.NRGBA{R: 37, G: 99, B: 235, A: 255}
			switch edge {
			case "top":
				fillTestRect(img, 100, 0, 140, 10, blue)
			case "bottom":
				fillTestRect(img, 100, 590, 140, 600, blue)
			case "left":
				fillTestRect(img, 0, 100, 10, 140, blue)
			case "right":
				fillTestRect(img, 790, 100, 800, 140, blue)
			}
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				t.Fatal(err)
			}
			_, _, issues := InspectPNGBytes(buf.Bytes(), nil)
			joined := strings.Join(issues, "; ")
			if !strings.Contains(joined, "png_clipped_edges") || !strings.Contains(joined, edge) {
				t.Fatalf("expected clipped edge %q, got %v", edge, issues)
			}
		})
	}

	// Corrupt PNG must fail (never silently swallowed).
	t.Run("CorruptPNGFailsViewReport", func(t *testing.T) {
		vr := &ViewReport{ID: "bad", Status: "PASS"}
		ValidateViewPNGReport(vr, []byte("not-a-valid-png"))
		if vr.Status != "FAIL" || len(vr.Overlaps) == 0 || !strings.Contains(vr.Overlaps[0], "png_decode_error") {
			t.Fatalf("expected png_decode_error FAIL on corrupt PNG, got %+v", vr)
		}
	})

	// Per-box blank check catches both NodeBoxes and BadgeBoxes.
	t.Run("BlankBadgeBoxCaught", func(t *testing.T) {
		raw := encodePartialTileBlankedPNG(t, 900, 600)
		vr := &ViewReport{
			ID:         "v1",
			BadgeBoxes: []Box{{ID: "hdr_blank", X: 120, Y: 260, Width: 200, Height: 40}},
			Status:     "PASS",
		}
		_, _, issues := InspectPNGBytes(raw, vr)
		if !strings.Contains(strings.Join(issues, "; "), "png_box_blank: hdr_blank") {
			t.Fatalf("expected png_box_blank for blank BadgeBox, got %v", issues)
		}
	})

	// InspectPNGFile streams directly from disk.
	t.Run("InspectPNGFileAndMissing", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "sample.png")
		if err := os.WriteFile(p, encodeTestPNG(t, 900, 600), 0o644); err != nil {
			t.Fatal(err)
		}
		w, h, issues := InspectPNGFile(p, nil)
		if w != 900 || h != 600 || len(issues) != 0 {
			t.Fatalf("InspectPNGFile valid failed: %dx%d issues=%v", w, h, issues)
		}
		_, _, missingIssues := InspectPNGFile(filepath.Join(t.TempDir(), "does-not-exist.png"), nil)
		if len(missingIssues) == 0 || !strings.Contains(missingIssues[0], "png_decode_error") {
			t.Fatalf("expected png_decode_error for missing file, got %v", missingIssues)
		}
	})
}
