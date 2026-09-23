// Package tui dibuja la salida de las herramientas con un lenguaje visual
// único: negro puro, acentos pastel, cero bordes duros, progreso vivo y una
// tarjeta de resumen al final. Si la salida no es una consola (pipe, archivo)
// degrada a texto plano sin un solo escape.
package tui

import "strconv"

// Color es un color de 24 bits. El valor cero significa "sin color" y deja el
// del terminal.
type Color struct {
	R, G, B uint8
	set     bool
}

// RGB arma un color explícito.
func RGB(r, g, b uint8) Color { return Color{R: r, G: g, B: b, set: true} }

// IsSet dice si el color fue definido.
func (c Color) IsSet() bool { return c.set }

// Lerp interpola entre c y d con t ∈ [0,1]. Se hace en sRGB y no en OKLab: la
// barra degrada entre dos pasteles vecinos, donde la diferencia no se ve, y así
// cada celda cuesta tres multiplicaciones en vez de trigonometría.
func (c Color) Lerp(d Color, t float64) Color {
	if !c.set {
		return d
	}
	if !d.set || t <= 0 {
		return c
	}
	if t >= 1 {
		return d
	}
	mix := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t + 0.5) }
	return RGB(mix(c.R, d.R), mix(c.G, d.G), mix(c.B, d.B))
}

// Acentos Catppuccin CLAROS sobre negro puro: la paleta elegida para las apps
// propias (los tonos saturados cansan la vista en una consola negra).
var (
	Teal     = RGB(0x8f, 0xd6, 0xcc)
	Lavender = RGB(0xc4, 0xb5, 0xfd)
	Peach    = RGB(0xf6, 0xc0, 0xa0)
	Pink     = RGB(0xf3, 0xb9, 0xd2)
	Sky      = RGB(0xa8, 0xcf, 0xf2)
	Cream    = RGB(0xee, 0xdf, 0xb8)
	Sage     = RGB(0xb5, 0xdf, 0xa8)
	Rose     = RGB(0xf2, 0xa7, 0xb8)

	Text   = RGB(0xcd, 0xd6, 0xf4) // cuerpo
	Subtle = RGB(0x8a, 0x8f, 0xa8) // etiquetas, contexto
	Faint  = RGB(0x55, 0x58, 0x6b) // separadores, pistas
	Ghost  = RGB(0x2c, 0x2e, 0x38) // pista vacía de la barra
	CardBG = RGB(0x10, 0x11, 0x16) // tinte de la tarjeta: apenas sobre el negro
)

// Accents es la rueda de acentos para puntos y chips, en un orden donde dos
// vecinos nunca se parecen.
var Accents = [...]Color{Teal, Lavender, Peach, Sky, Pink, Sage, Cream}

// Accent devuelve el i-ésimo acento de la rueda (cíclico).
func Accent(i int) Color {
	if i < 0 {
		i = -i
	}
	return Accents[i%len(Accents)]
}

func appendFG(b []byte, c Color) []byte {
	b = append(b, "\x1b[38;2;"...)
	return appendTriplet(b, c)
}

func appendBG(b []byte, c Color) []byte {
	b = append(b, "\x1b[48;2;"...)
	return appendTriplet(b, c)
}

func appendTriplet(b []byte, c Color) []byte {
	b = strconv.AppendUint(b, uint64(c.R), 10)
	b = append(b, ';')
	b = strconv.AppendUint(b, uint64(c.G), 10)
	b = append(b, ';')
	b = strconv.AppendUint(b, uint64(c.B), 10)
	return append(b, 'm')
}

const resetSeq = "\x1b[0m"
