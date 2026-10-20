package main

import (
	"bytes"
	"embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/png"
)

//go:embed assets/*.png
var assetsFS embed.FS

var iconSizes = []int{16, 20, 24, 32, 40, 48}

// pickSize выбирает ближайший доступный размер не меньше нужного.
func pickSize(want int) int {
	for _, s := range iconSizes {
		if s >= want {
			return s
		}
	}
	return iconSizes[len(iconSizes)-1]
}

// iconImage превращает PNG в данные значка (BITMAPINFOHEADER + 32-битные пиксели BGRA + AND-маска)
// в формате, который понимает CreateIconFromResourceEx.
func iconImage(pngData []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), src, b.Min, draw.Src)

	var out bytes.Buffer
	le := binary.LittleEndian
	hdr := []any{uint32(40), int32(w), int32(h * 2), uint16(1), uint16(32), uint32(0), uint32(0), int32(0), int32(0), uint32(0), uint32(0)}
	for _, v := range hdr {
		_ = binary.Write(&out, le, v)
	}
	for y := h - 1; y >= 0; y-- { // строки снизу вверх
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			out.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	out.Write(make([]byte, ((w+31)/32*4)*h)) // AND-маска (прозрачность берётся из альфа-канала)
	return out.Bytes(), nil
}

// iconSet — готовые данные значков одного размера:
// on — рабочий режим, active — идёт синхронизация (мигает с on), paused — пауза, err — ошибка (on с красным бейджем).
type iconSet struct {
	size   int
	on     []byte
	active []byte
	paused []byte
	err    []byte
}

func loadIconImage(name string) ([]byte, error) {
	data, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		return nil, err
	}
	return iconImage(data)
}

func loadIconSet(want int) (*iconSet, error) {
	s := pickSize(want)
	set := &iconSet{size: s}
	for name, dst := range map[string]*[]byte{"on": &set.on, "active": &set.active, "paused": &set.paused, "error": &set.err} {
		img, err := loadIconImage(fmt.Sprintf("%s_%d.png", name, s))
		if err != nil {
			return nil, err
		}
		*dst = img
	}
	return set, nil
}
