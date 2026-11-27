package render

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"strings"
)

const (
	pngMinNonBGRatio       = 0.005
	pngMaxNonBGRatio       = 0.90
	pngMinHighContrast     = 0.002
	pngMinChromaticRatio   = 0.001
	pngMinDistinctColors   = 8
	pngMinBoxSizePx        = 8
	pngMinBoxDistinctColor = 3
	pngBGColorDelta        = 16
	pngHighContrastDelta   = 48
	pngChromaDelta         = 16
	pngInteriorGridSample  = 16
	pngClipRatioDenom      = 1000
	pngSignature           = "\x89PNG\r\n\x1a\n"
)

var errFallbackPNGDecode = errors.New("non-8-bit or interlaced PNG")

type pngHeader struct {
	width      int
	height     int
	colorType  byte
	bpp        int
	palette    [256]color.NRGBA
	paletteLen int
	hasTRNS    bool
	trnsGray   uint8
	trnsR      uint8
	trnsG      uint8
	trnsB      uint8
	idatLen    uint32
	idatCRC    hash.Hash32
}

// InspectPNGBytes verifies PNG dimensions, aspect ratio, pixel content diversity,
// 1px border clipping, and optional per-box non-blankness (NodeBoxes and BadgeBoxes)
// using a streaming scanline decoder (< 1 MB RAM for 8-bit non-interlaced PNGs).
func InspectPNGBytes(pngBytes []byte, vr *ViewReport) (width, height int, issues []string) {
	return inspectPNGStream(bytes.NewReader(pngBytes), vr)
}

// InspectPNGFile opens path and streams it directly through the scanline PNG verifier.
func InspectPNGFile(path string, vr *ViewReport) (width, height int, issues []string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, []string{fmt.Sprintf("png_decode_error: %v", err)}
	}
	defer func() { _ = f.Close() }()
	return inspectPNGStream(f, vr)
}

// ValidateViewPNGReport updates vr dimensions (when unset), runs streaming PNG pixel and
// per-box verification, and records any failures (including decode errors) on vr.
func ValidateViewPNGReport(vr *ViewReport, pngBytes []byte) {
	if vr == nil || len(pngBytes) == 0 {
		return
	}
	w, h, issues := InspectPNGBytes(pngBytes, vr)
	applyPNGVerificationToReport(vr, w, h, issues)
}

// ValidateViewPNGFile streams path from disk through the PNG verifier and records
// dimensions and any failures on vr.
func ValidateViewPNGFile(vr *ViewReport, path string) {
	if vr == nil || path == "" {
		return
	}
	w, h, issues := InspectPNGFile(path, vr)
	applyPNGVerificationToReport(vr, w, h, issues)
}

func applyPNGVerificationToReport(vr *ViewReport, w, h int, issues []string) {
	if w > 0 && h > 0 && (vr.WidthPx == 0 || vr.HeightPx == 0) {
		vr.WidthPx = w
		vr.HeightPx = h
		vr.AspectRatio = math.Round((float64(w)/float64(h))*1000) / 1000
	}
	for _, msg := range issues {
		recordViewFailure(vr, msg)
	}
}

func inspectPNGStream(rs io.ReadSeeker, vr *ViewReport) (int, int, []string) {
	an, err := runStreamingPass(rs, vr, color.NRGBA{}, false)
	if errors.Is(err, errFallbackPNGDecode) {
		return inspectPNGFallback(rs, vr)
	}
	if err != nil {
		return 0, 0, []string{fmt.Sprintf("png_decode_error: %v", err)}
	}
	if detected := an.bgSamples.dominant(); detected != an.bg {
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			return 0, 0, []string{fmt.Sprintf("png_decode_error: rewind for pass 2: %v", err)}
		}
		an, err = runStreamingPass(rs, vr, detected, true)
		if err != nil {
			return 0, 0, []string{fmt.Sprintf("png_decode_error: %v", err)}
		}
	}
	return an.w, an.h, an.buildIssues()
}

func inspectPNGFallback(rs io.ReadSeeker, vr *ViewReport) (int, int, []string) {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return 0, 0, []string{fmt.Sprintf("png_decode_error: rewind fallback: %v", err)}
	}
	img, err := png.Decode(rs)
	if err != nil {
		return 0, 0, []string{fmt.Sprintf("png_decode_error: %v", err)}
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return w, h, []string{fmt.Sprintf("png_dimensions %dx%d below %dx%d", w, h, PNGMinWidth, PNGMinHeight)}
	}

	an := analyzeImageRows(img, b, vr, color.NRGBA{}, false)
	if detected := an.bgSamples.dominant(); detected != an.bg {
		an = analyzeImageRows(img, b, vr, detected, true)
	}
	return w, h, an.buildIssues()
}

func analyzeImageRows(img image.Image, b image.Rectangle, vr *ViewReport, knownBG color.NRGBA, hasKnownBG bool) *pngRowAnalyzer {
	w, h := b.Dx(), b.Dy()
	an := newPNGRowAnalyzer(w, h, vr, knownBG, hasKnownBG)
	hdr := pngHeader{width: w, height: h, colorType: 6, bpp: 4}
	rowBuf := make([]byte, w*4)
	for y := range h {
		for x := range w {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			off := x * 4
			rowBuf[off] = c.R
			rowBuf[off+1] = c.G
			rowBuf[off+2] = c.B
			rowBuf[off+3] = c.A
		}
		an.consumeRow(y, rowBuf, &hdr)
	}
	return an
}

func runStreamingPass(rs io.Reader, vr *ViewReport, knownBG color.NRGBA, hasKnownBG bool) (*pngRowAnalyzer, error) {
	br := bufio.NewReaderSize(rs, 32*1024)
	hdr, err := parsePNGHeader(br)
	if err != nil {
		return nil, err
	}

	rowBytes := hdr.width * hdr.bpp
	if rowBytes <= 0 || rowBytes/hdr.bpp != hdr.width {
		return nil, fmt.Errorf("invalid row stride for %dx%d", hdr.width, hdr.height)
	}

	idatR := &idatStreamReader{r: br, rem: hdr.idatLen, crc: hdr.idatCRC}
	zr, err := zlib.NewReader(idatR)
	if err != nil {
		return nil, fmt.Errorf("init zlib: %w", err)
	}
	defer func() { _ = zr.Close() }()

	an := newPNGRowAnalyzer(hdr.width, hdr.height, vr, knownBG, hasKnownBG)
	bufA := make([]byte, 1+rowBytes)
	bufB := make([]byte, 1+rowBytes)
	prev := bufB[1:]

	for y := range hdr.height {
		if _, err := io.ReadFull(zr, bufA); err != nil {
			return nil, fmt.Errorf("read scanline %d: %w", y, err)
		}
		cur := bufA[1:]
		if err := unfilterScanline(bufA[0], cur, prev, hdr.bpp); err != nil {
			return nil, fmt.Errorf("scanline %d: %w", y, err)
		}
		an.consumeRow(y, cur, hdr)
		bufA, bufB = bufB, bufA
		prev = cur
	}

	// Drain to EOF so zlib verifies its Adler-32 checksum.
	var extra [1]byte
	if n, err := zr.Read(extra[:]); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("verify zlib stream: %w", err)
	} else if n > 0 {
		return nil, fmt.Errorf("unexpected trailing scanline data")
	}
	if err := zr.Close(); err != nil {
		return nil, fmt.Errorf("close zlib: %w", err)
	}
	if err := idatR.finish(); err != nil {
		return nil, err
	}
	return an, nil
}

func parsePNGHeader(br *bufio.Reader) (*pngHeader, error) {
	var sig [8]byte
	if _, err := io.ReadFull(br, sig[:]); err != nil {
		return nil, fmt.Errorf("read png signature: %w", err)
	}
	if string(sig[:]) != pngSignature {
		return nil, fmt.Errorf("invalid png signature")
	}

	hdr := &pngHeader{}
	ihdrSeen := false
	for {
		done, err := parsePNGChunk(br, hdr, &ihdrSeen)
		if err != nil {
			return nil, err
		}
		if done {
			return hdr, nil
		}
	}
}

func readPNGChunkHeader(br *bufio.Reader) (string, uint32, [4]byte, error) {
	var chunkHdr [8]byte
	if _, err := io.ReadFull(br, chunkHdr[:]); err != nil {
		return "", 0, [4]byte{}, fmt.Errorf("read chunk header: %w", err)
	}
	length := binary.BigEndian.Uint32(chunkHdr[:4])
	if length > 0x7fffffff {
		return "", 0, [4]byte{}, fmt.Errorf("png chunk length %d exceeds limit", length)
	}
	var rawType [4]byte
	copy(rawType[:], chunkHdr[4:8])
	return string(rawType[:]), length, rawType, nil
}

func parsePNGChunk(br *bufio.Reader, hdr *pngHeader, ihdrSeen *bool) (bool, error) {
	typ, length, rawType, err := readPNGChunkHeader(br)
	if err != nil {
		return false, err
	}
	if typ == "IHDR" {
		if *ihdrSeen {
			return false, fmt.Errorf("duplicate IHDR chunk")
		}
		if err := parseIHDRChunk(br, length, hdr); err != nil {
			return false, err
		}
		*ihdrSeen = true
		return false, nil
	}
	if typ == "IEND" {
		return false, fmt.Errorf("png missing IDAT chunk")
	}
	if !*ihdrSeen {
		return false, fmt.Errorf("expected IHDR first, got %q", typ)
	}

	switch typ {
	case "PLTE":
		return false, parsePLTEChunk(br, length, hdr)
	case "tRNS":
		return false, parseTRNSChunk(br, length, hdr)
	case "IDAT":
		if hdr.colorType == 3 && hdr.paletteLen == 0 {
			return false, fmt.Errorf("indexed png missing PLTE")
		}
		hdr.idatLen = length
		hdr.idatCRC = crc32.NewIEEE()
		_, _ = hdr.idatCRC.Write(rawType[:])
		return true, nil
	default:
		if _, err := io.CopyN(io.Discard, br, int64(length)+4); err != nil {
			return false, fmt.Errorf("skip chunk %q: %w", typ, err)
		}
		return false, nil
	}
}

func readCheckedChunk(br *bufio.Reader, typ string, length uint32) ([]byte, error) {
	buf := make([]byte, length+4)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, fmt.Errorf("read %s chunk: %w", typ, err)
	}
	payload := buf[:length]
	wantCRC := binary.BigEndian.Uint32(buf[length:])
	h := crc32.NewIEEE()
	_, _ = h.Write([]byte(typ))
	_, _ = h.Write(payload)
	if h.Sum32() != wantCRC {
		return nil, fmt.Errorf("%s chunk CRC mismatch", typ)
	}
	return payload, nil
}

func parseIHDRChunk(br *bufio.Reader, length uint32, hdr *pngHeader) error {
	if length != 13 {
		return fmt.Errorf("invalid IHDR length %d", length)
	}
	data, err := readCheckedChunk(br, "IHDR", length)
	if err != nil {
		return err
	}
	w := int(binary.BigEndian.Uint32(data[0:4]))
	h := int(binary.BigEndian.Uint32(data[4:8]))
	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid png dimensions %dx%d", w, h)
	}
	bitDepth := data[8]
	colorType := data[9]
	compression := data[10]
	filterMethod := data[11]
	interlace := data[12]
	if compression != 0 || filterMethod != 0 || (interlace != 0 && interlace != 1) {
		return fmt.Errorf("unsupported png header flags")
	}
	if bitDepth != 8 || interlace != 0 {
		return errFallbackPNGDecode
	}
	bpp, err := pngBytesPerPixel(colorType)
	if err != nil {
		return err
	}
	hdr.width = w
	hdr.height = h
	hdr.colorType = colorType
	hdr.bpp = bpp
	return nil
}

func pngBytesPerPixel(colorType byte) (int, error) {
	switch colorType {
	case 0, 3:
		return 1, nil
	case 2:
		return 3, nil
	case 4:
		return 2, nil
	case 6:
		return 4, nil
	default:
		return 0, fmt.Errorf("unsupported png color type %d", colorType)
	}
}

func parsePLTEChunk(br *bufio.Reader, length uint32, hdr *pngHeader) error {
	if length == 0 || length%3 != 0 || length > 768 {
		return fmt.Errorf("invalid PLTE length %d", length)
	}
	data, err := readCheckedChunk(br, "PLTE", length)
	if err != nil {
		return err
	}
	n := len(data) / 3
	for i := range n {
		hdr.palette[i] = color.NRGBA{R: data[i*3], G: data[i*3+1], B: data[i*3+2], A: 255}
	}
	hdr.paletteLen = n
	return nil
}

func parseTRNSChunk(br *bufio.Reader, length uint32, hdr *pngHeader) error {
	if length > 256 {
		return fmt.Errorf("invalid tRNS length %d", length)
	}
	data, err := readCheckedChunk(br, "tRNS", length)
	if err != nil {
		return err
	}
	switch hdr.colorType {
	case 0:
		if len(data) != 2 {
			return fmt.Errorf("invalid grayscale tRNS length %d", len(data))
		}
		hdr.hasTRNS = true
		hdr.trnsGray = uint8(binary.BigEndian.Uint16(data))
	case 2:
		if len(data) != 6 {
			return fmt.Errorf("invalid RGB tRNS length %d", len(data))
		}
		hdr.hasTRNS = true
		hdr.trnsR = uint8(binary.BigEndian.Uint16(data[0:2]))
		hdr.trnsG = uint8(binary.BigEndian.Uint16(data[2:4]))
		hdr.trnsB = uint8(binary.BigEndian.Uint16(data[4:6]))
	case 3:
		if len(data) > hdr.paletteLen {
			return fmt.Errorf("tRNS exceeds palette size")
		}
		for i, a := range data {
			hdr.palette[i].A = a
		}
	default:
		return fmt.Errorf("tRNS not allowed for color type %d", hdr.colorType)
	}
	return nil
}

type idatStreamReader struct {
	r    *bufio.Reader
	rem  uint32
	crc  hash.Hash32
	done bool
}

func (s *idatStreamReader) Read(p []byte) (int, error) {
	for !s.done {
		if s.rem > 0 {
			want := p
			if uint32(len(want)) > s.rem {
				want = p[:s.rem]
			}
			n, err := s.r.Read(want)
			if n > 0 {
				_, _ = s.crc.Write(want[:n])
				s.rem -= uint32(n)
				return n, nil
			}
			if err != nil {
				return 0, err
			}
		}
		if err := s.verifyAndAdvance(); err != nil {
			return 0, err
		}
	}
	return 0, io.EOF
}

func (s *idatStreamReader) verifyAndAdvance() error {
	if err := s.verifyChunkCRC(); err != nil {
		return err
	}
	var hdr [8]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return fmt.Errorf("read next png chunk after IDAT: %w", err)
	}
	length := binary.BigEndian.Uint32(hdr[:4])
	typ := string(hdr[4:8])
	if typ == "IDAT" {
		s.crc.Reset()
		_, _ = s.crc.Write(hdr[4:8])
		s.rem = length
		return nil
	}
	s.done = true
	return nil
}

func (s *idatStreamReader) verifyChunkCRC() error {
	var crcBuf [4]byte
	if _, err := io.ReadFull(s.r, crcBuf[:]); err != nil {
		return fmt.Errorf("read IDAT CRC: %w", err)
	}
	if binary.BigEndian.Uint32(crcBuf[:]) != s.crc.Sum32() {
		return fmt.Errorf("IDAT chunk CRC mismatch")
	}
	return nil
}

func (s *idatStreamReader) finish() error {
	if s.done {
		return nil
	}
	if s.rem > 0 {
		if _, err := io.CopyN(s.crc, s.r, int64(s.rem)); err != nil {
			return fmt.Errorf("drain IDAT payload: %w", err)
		}
		s.rem = 0
	}
	s.done = true
	return s.verifyChunkCRC()
}

func unfilterScanline(filterType byte, cur, prev []byte, bpp int) error {
	n := len(cur)
	if n == 0 {
		return nil
	}
	prev = prev[:n]
	switch filterType {
	case 0: // None
		return nil
	case 1: // Sub
		for i := bpp; i < n; i++ {
			cur[i] += cur[i-bpp]
		}
	case 2: // Up
		for i := range n {
			cur[i] += prev[i]
		}
	case 3: // Average
		for i := range bpp {
			cur[i] += prev[i] >> 1
		}
		for i := bpp; i < n; i++ {
			cur[i] += uint8((int(cur[i-bpp]) + int(prev[i])) >> 1)
		}
	case 4: // Paeth (branchless RFC 2083 predictor to keep tight loops fast and branch-free)
		for i := range bpp {
			cur[i] += prev[i]
		}
		for i := bpp; i < n; i++ {
			a, b, c := int32(cur[i-bpp]), int32(prev[i]), int32(prev[i-bpp])
			pa := b - c
			pb := a - c
			pc := pa + pb
			ma := pa >> 31
			pa = (pa ^ ma) - ma
			mb := pb >> 31
			pb = (pb ^ mb) - mb
			mc := pc >> 31
			pc = (pc ^ mc) - mc
			useA := ((pa - pb - 1) & (pa - pc - 1)) >> 31
			useB := ^useA & ((pb - pc - 1) >> 31)
			useC := ^(useA | useB)
			cur[i] += uint8((a & useA) | (b & useB) | (c & useC))
		}
	default:
		return fmt.Errorf("invalid PNG filter type %d", filterType)
	}
	return nil
}

type bgSampleCollector struct {
	w, h   int
	stepX0 int
	stepY0 int
	stepX2 int
	stepY2 int
	gridX  [pngInteriorGridSample]int
	gridY  [pngInteriorGridSample]int
	counts map[uint32]int
}

func newBGSampleCollector(w, h int) bgSampleCollector {
	c := bgSampleCollector{
		w:      w,
		h:      h,
		stepX0: max(1, w/50),
		stepY0: max(1, h/50),
		counts: make(map[uint32]int, 64),
	}
	if w > 4 && h > 4 {
		c.stepX2 = max(1, (w-4)/50)
		c.stepY2 = max(1, (h-4)/50)
	}
	for i := range pngInteriorGridSample {
		c.gridX[i] = (w * (i + 1)) / (pngInteriorGridSample + 1)
		c.gridY[i] = (h * (i + 1)) / (pngInteriorGridSample + 1)
	}
	return c
}

func (c *bgSampleCollector) add(px color.NRGBA) {
	c.counts[packPNGNRGBA(px)]++
}

func (c *bgSampleCollector) sampleRow(y int, cur []byte, hdr *pngHeader) {
	// Outer ring.
	if y == 0 || y == c.h-1 {
		for x := 0; x < c.w; x += c.stepX0 {
			c.add(decodeRowPixel(cur, x, hdr))
		}
	}
	if y%c.stepY0 == 0 {
		c.add(decodeRowPixel(cur, 0, hdr))
		c.add(decodeRowPixel(cur, c.w-1, hdr))
	}
	if y == c.h-1 {
		c.add(decodeRowPixel(cur, c.w-1, hdr))
	}

	// 2px inset ring.
	if c.w > 4 && c.h > 4 {
		if y == 2 || y == c.h-3 {
			for x := 2; x < c.w-2; x += c.stepX2 {
				c.add(decodeRowPixel(cur, x, hdr))
			}
		}
		if y >= 2 && y < c.h-2 && (y-2)%c.stepY2 == 0 {
			c.add(decodeRowPixel(cur, 2, hdr))
			c.add(decodeRowPixel(cur, c.w-3, hdr))
		}
		if y == c.h-3 {
			c.add(decodeRowPixel(cur, c.w-3, hdr))
		}
	}

	// 16x16 interior grid.
	for _, gy := range c.gridY {
		if y == gy {
			for _, gx := range c.gridX {
				c.add(decodeRowPixel(cur, gx, hdr))
			}
		}
	}
}

func (c *bgSampleCollector) dominant() color.NRGBA {
	var bestKey uint32
	bestCount := -1
	for k, cnt := range c.counts {
		if cnt > bestCount || (cnt == bestCount && k < bestKey) {
			bestCount = cnt
			bestKey = k
		}
	}
	return unpackPNGNRGBA(bestKey)
}

type boxTracker struct {
	id         string
	x0, y0     int
	x1, y1     int
	seenColors [pngMinBoxDistinctColor]uint16
	seenCount  uint8
	hasNonBG   bool
	done       bool
}

func (bt *boxTracker) record(c, bg color.NRGBA) {
	if !bt.hasNonBG && pngColorDiff(c, bg) > pngBGColorDelta {
		bt.hasNonBG = true
	}
	q := packPNGQuantized4(c)
	found := false
	for i := range bt.seenCount {
		if bt.seenColors[i] == q {
			found = true
			break
		}
	}
	if !found && int(bt.seenCount) < len(bt.seenColors) {
		bt.seenColors[bt.seenCount] = q
		bt.seenCount++
	}
	if bt.hasNonBG && int(bt.seenCount) >= pngMinBoxDistinctColor {
		bt.done = true
	}
}

type pngRowAnalyzer struct {
	w, h         int
	stride       int
	sampled      int
	bg           color.NRGBA
	hasKnownBG   bool
	bgSamples    bgSampleCollector
	nonBG        int
	highContrast int
	chromatic    int
	seen         [4096]bool
	distinct     int
	top0, bot0   int
	left0        int
	right0       int
	top1, bot1   int
	left1        int
	right1       int
	boxes        []boxTracker
}

func newPNGRowAnalyzer(w, h int, vr *ViewReport, knownBG color.NRGBA, hasKnownBG bool) *pngRowAnalyzer {
	stride := 1
	if area := w * h; area > 8_000_000 {
		stride = 4
	} else if area > 2_000_000 {
		stride = 2
	}
	an := &pngRowAnalyzer{
		w:          w,
		h:          h,
		stride:     stride,
		bg:         knownBG,
		hasKnownBG: hasKnownBG,
	}
	if !hasKnownBG {
		an.bgSamples = newBGSampleCollector(w, h)
	}
	if vr != nil {
		an.boxes = make([]boxTracker, 0, len(vr.NodeBoxes)+len(vr.BadgeBoxes))
		an.appendBoxes(vr.NodeBoxes)
		an.appendBoxes(vr.BadgeBoxes)
	}
	return an
}

func (a *pngRowAnalyzer) appendBoxes(boxes []Box) {
	for _, box := range boxes {
		x0 := max(0, int(math.Floor(box.X)))
		y0 := max(0, int(math.Floor(box.Y)))
		x1 := min(a.w, int(math.Ceil(box.X+box.Width)))
		y1 := min(a.h, int(math.Ceil(box.Y+box.Height)))
		if x1-x0 < pngMinBoxSizePx || y1-y0 < pngMinBoxSizePx {
			continue
		}
		a.boxes = append(a.boxes, boxTracker{id: box.ID, x0: x0, y0: y0, x1: x1, y1: y1})
	}
}

func (a *pngRowAnalyzer) consumeRow(y int, cur []byte, hdr *pngHeader) {
	if !a.hasKnownBG {
		if y == 0 {
			a.bg = dominantRowColor(cur, hdr)
		}
		a.bgSamples.sampleRow(y, cur, hdr)
	}
	if y%a.stride == 0 {
		a.scanRowContent(cur, hdr)
	}
	a.scanRowBorders(y, cur, hdr)
	a.scanRowBoxes(y, cur, hdr)
}

func (a *pngRowAnalyzer) scanRowContent(cur []byte, hdr *pngHeader) {
	bg := a.bg
	step := a.stride
	var sampled, nonBG, highContrast, chromatic, distinct int
	seen := &a.seen

	switch {
	case hdr.colorType == 6:
		byteStep := step * 4
		for i := 0; i < len(cur); i += byteStep {
			sampled++
			r, g, b, al := cur[i], cur[i+1], cur[i+2], cur[i+3]
			if r == bg.R && g == bg.G && b == bg.B && al == bg.A {
				continue
			}
			accumulateRowPixel(color.NRGBA{R: r, G: g, B: b, A: al}, bg, seen, &nonBG, &highContrast, &chromatic, &distinct)
		}
	case hdr.colorType == 2 && !hdr.hasTRNS:
		byteStep := step * 3
		for i := 0; i < len(cur); i += byteStep {
			sampled++
			r, g, b := cur[i], cur[i+1], cur[i+2]
			if bg.A == 255 && r == bg.R && g == bg.G && b == bg.B {
				continue
			}
			accumulateRowPixel(color.NRGBA{R: r, G: g, B: b, A: 255}, bg, seen, &nonBG, &highContrast, &chromatic, &distinct)
		}
	default:
		for x := 0; x < a.w; x += step {
			sampled++
			c := decodeRowPixel(cur, x, hdr)
			if c == bg {
				continue
			}
			accumulateRowPixel(c, bg, seen, &nonBG, &highContrast, &chromatic, &distinct)
		}
	}

	a.sampled += sampled
	a.nonBG += nonBG
	a.highContrast += highContrast
	a.chromatic += chromatic
	a.distinct += distinct
}

func accumulateRowPixel(c, bg color.NRGBA, seen *[4096]bool, nonBG, highContrast, chromatic, distinct *int) {
	diff := pngColorDiff(c, bg)
	if diff <= pngBGColorDelta {
		return
	}
	*nonBG++
	q := packPNGQuantized4(c)
	if !seen[q] {
		seen[q] = true
		*distinct++
	}
	if pngColorChroma(c) >= pngChromaDelta {
		*chromatic++
	}
	if diff >= pngHighContrastDelta {
		*highContrast++
	}
}

func (a *pngRowAnalyzer) countRowBorderHits(cur []byte, hdr *pngHeader, x0, x1 int) int {
	hits := 0
	for x := x0; x < x1; x++ {
		if pngColorDiff(decodeRowPixel(cur, x, hdr), a.bg) > pngBGColorDelta {
			hits++
		}
	}
	return hits
}

func (a *pngRowAnalyzer) isBorderHit(cur []byte, hdr *pngHeader, x int) int {
	if pngColorDiff(decodeRowPixel(cur, x, hdr), a.bg) > pngBGColorDelta {
		return 1
	}
	return 0
}

func (a *pngRowAnalyzer) scanRowBorders(y int, cur []byte, hdr *pngHeader) {
	if y == 0 {
		a.top0 += a.countRowBorderHits(cur, hdr, 0, a.w)
	}
	if y == a.h-1 {
		a.bot0 += a.countRowBorderHits(cur, hdr, 0, a.w)
	}
	a.left0 += a.isBorderHit(cur, hdr, 0)
	a.right0 += a.isBorderHit(cur, hdr, a.w-1)

	if a.w < 10 || a.h < 10 {
		return
	}
	if y == 1 {
		a.top1 += a.countRowBorderHits(cur, hdr, 1, a.w-1)
	}
	if y == a.h-2 {
		a.bot1 += a.countRowBorderHits(cur, hdr, 1, a.w-1)
	}
	if y >= 1 && y <= a.h-2 {
		a.left1 += a.isBorderHit(cur, hdr, 1)
		a.right1 += a.isBorderHit(cur, hdr, a.w-2)
	}
}

func (a *pngRowAnalyzer) scanRowBoxes(y int, cur []byte, hdr *pngHeader) {
	bg := a.bg
	for i := range a.boxes {
		bt := &a.boxes[i]
		if bt.done || y < bt.y0 || y >= bt.y1 {
			continue
		}
		for x := bt.x0; x < bt.x1; x++ {
			bt.record(decodeRowPixel(cur, x, hdr), bg)
			if bt.done {
				break
			}
		}
	}
}

func (a *pngRowAnalyzer) clippedEdges() []string {
	topHits, botHits, leftHits, rightHits := a.top0, a.bot0, a.left0, a.right0
	ew, eh := a.w, a.h
	if a.w >= 10 && a.h >= 10 &&
		topHits >= a.w-2 && botHits >= a.w-2 &&
		leftHits >= a.h-2 && rightHits >= a.h-2 {
		topHits, botHits, leftHits, rightHits = a.top1, a.bot1, a.left1, a.right1
		ew, eh = a.w-2, a.h-2
	}
	edges := []struct {
		name string
		hits int
		len  int
	}{
		{name: "top", hits: topHits, len: ew},
		{name: "bottom", hits: botHits, len: ew},
		{name: "left", hits: leftHits, len: eh},
		{name: "right", hits: rightHits, len: eh},
	}
	var clipped []string
	for _, e := range edges {
		limit := max(2, e.len/pngClipRatioDenom)
		if e.hits > limit {
			clipped = append(clipped, e.name)
		}
	}
	return clipped
}

func (a *pngRowAnalyzer) buildIssues() []string {
	var issues []string
	if a.w < PNGMinWidth || a.h < PNGMinHeight {
		issues = append(issues, fmt.Sprintf("png_dimensions %dx%d below %dx%d", a.w, a.h, PNGMinWidth, PNGMinHeight))
	}
	if a.w <= 0 || a.h <= 0 {
		return issues
	}

	rawAR := float64(a.w) / float64(a.h)
	roundedAR := math.Round(rawAR*1000) / 1000
	if rawAR < PNGMinAspectRatio || rawAR > PNGMaxAspectRatio {
		issues = append(issues, fmt.Sprintf("aspect_ratio %.3f outside [%.2f, %.2f]", roundedAR, PNGMinAspectRatio, PNGMaxAspectRatio))
	}

	ftotal := float64(max(1, a.sampled))
	nonBG := float64(a.nonBG) / ftotal
	highContrast := float64(a.highContrast) / ftotal
	chromaRatio := float64(a.chromatic) / ftotal

	if nonBG < pngMinNonBGRatio {
		issues = append(issues, fmt.Sprintf("png_blank: non-bg ratio %.4f < %.3f", nonBG, pngMinNonBGRatio))
	}
	if nonBG > pngMaxNonBGRatio {
		issues = append(issues, fmt.Sprintf("png_solid: non-bg ratio %.4f > %.2f", nonBG, pngMaxNonBGRatio))
	}
	if highContrast < pngMinHighContrast {
		issues = append(issues, fmt.Sprintf("png_low_contrast: high-contrast ratio %.4f < %.3f", highContrast, pngMinHighContrast))
	}
	if a.distinct < pngMinDistinctColors {
		issues = append(issues, fmt.Sprintf("png_low_color_diversity: %d < %d distinct colors", a.distinct, pngMinDistinctColors))
	}
	if chromaRatio < pngMinChromaticRatio {
		issues = append(issues, fmt.Sprintf("png_monochrome: chromatic ratio %.4f < %.3f", chromaRatio, pngMinChromaticRatio))
	}
	if clipped := a.clippedEdges(); len(clipped) > 0 {
		issues = append(issues, fmt.Sprintf("png_clipped_edges: %s", strings.Join(clipped, ", ")))
	}
	for _, bt := range a.boxes {
		if !bt.done {
			issues = append(issues, fmt.Sprintf("png_box_blank: %s (%dx%d distinct=%d)", bt.id, bt.x1-bt.x0, bt.y1-bt.y0, bt.seenCount))
		}
	}
	return issues
}

func dominantRowColor(cur []byte, hdr *pngHeader) color.NRGBA {
	counts := make(map[uint32]int, 16)
	step := max(1, hdr.width/200)
	for x := 0; x < hdr.width; x += step {
		counts[packPNGNRGBA(decodeRowPixel(cur, x, hdr))]++
	}
	var bestKey uint32
	bestCount := -1
	for k, cnt := range counts {
		if cnt > bestCount || (cnt == bestCount && k < bestKey) {
			bestCount = cnt
			bestKey = k
		}
	}
	return unpackPNGNRGBA(bestKey)
}

func decodeRowPixel(cur []byte, x int, hdr *pngHeader) color.NRGBA {
	switch hdr.colorType {
	case 6:
		off := x * 4
		return color.NRGBA{R: cur[off], G: cur[off+1], B: cur[off+2], A: cur[off+3]}
	case 2:
		off := x * 3
		r, g, b := cur[off], cur[off+1], cur[off+2]
		a := uint8(255)
		if hdr.hasTRNS && r == hdr.trnsR && g == hdr.trnsG && b == hdr.trnsB {
			a = 0
		}
		return color.NRGBA{R: r, G: g, B: b, A: a}
	case 3:
		idx := int(cur[x])
		if idx < hdr.paletteLen {
			return hdr.palette[idx]
		}
		return color.NRGBA{}
	case 0:
		v := cur[x]
		a := uint8(255)
		if hdr.hasTRNS && v == hdr.trnsGray {
			a = 0
		}
		return color.NRGBA{R: v, G: v, B: v, A: a}
	case 4:
		off := x * 2
		v := cur[off]
		return color.NRGBA{R: v, G: v, B: v, A: cur[off+1]}
	default:
		return color.NRGBA{}
	}
}

func packPNGNRGBA(c color.NRGBA) uint32 {
	return uint32(c.R)<<24 | uint32(c.G)<<16 | uint32(c.B)<<8 | uint32(c.A)
}

func unpackPNGNRGBA(k uint32) color.NRGBA {
	return color.NRGBA{
		R: uint8(k >> 24),
		G: uint8(k >> 16),
		B: uint8(k >> 8),
		A: uint8(k),
	}
}

func packPNGQuantized4(c color.NRGBA) uint16 {
	return uint16(c.R>>4)<<8 | uint16(c.G>>4)<<4 | uint16(c.B>>4)
}

func pngColorChroma(c color.NRGBA) int {
	hi := max(c.R, max(c.G, c.B))
	lo := min(c.R, min(c.G, c.B))
	return int(hi - lo)
}

func pngColorDiff(a, b color.NRGBA) int {
	dr := pngAbsDiff(a.R, b.R)
	dg := pngAbsDiff(a.G, b.G)
	db := pngAbsDiff(a.B, b.B)
	da := pngAbsDiff(a.A, b.A)
	return max(max(dr, dg), max(db, da))
}

func pngAbsDiff(a, b uint8) int {
	if a >= b {
		return int(a - b)
	}
	return int(b - a)
}
