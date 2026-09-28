package ports

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// resumenReservas aplana las reservas a "url=sid,sid".
func resumenReservas(rs []URLReservation) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.URL.String()+"="+strings.Join(r.SIDs, ","))
	}
	return out
}

const (
	sidWinRM  = "S-1-5-80-569256582-2953403351-2909559716-1301513147-412116970"
	sidWecsvc = "S-1-5-80-4059739203-877974739-1245631912-527174227-2996563517"
)

// La muestra "es" es la salida REAL de `netsh http show urlacl` en esta
// máquina (Windows 11 28120 en español), menos una reserva que nombraba a la
// cuenta del usuario. Son las de fábrica de Windows y de sus servicios.
func TestReservasMuestraReal(t *testing.T) {
	out := leerColas(t, "netsh_es_urlacl.txt")
	lf := strings.ReplaceAll(out, "\r\n", "\n")
	want := []string{
		"http://*:5357/=BU,LS",
		"http://+:80/Temporary_Listen_Addresses/=WD",
		"https://*:5358/=BU,LS",
		"http://*:2869/=LS",
		"https://+:5986/wsman/=" + sidWinRM + "," + sidWecsvc,
		"http://+:47001/wsman/=" + sidWinRM + "," + sidWecsvc,
		"http://+:5985/wsman/=" + sidWinRM + "," + sidWecsvc,
		"http://+:8733/Design_Time_Addresses/=IU",
		"http://+:12292/=SY",
		"https://+:10245/WMPNSSv4/=S-1-5-80-2375682873-768044350-3534595160-1005545032-2873800392",
		"http://+:10243/WMPNSSv4/=S-1-5-80-2375682873-768044350-3534595160-1005545032-2873800392",
		"http://+:10247/apps/=AU",
		"https://+:3392/rdp/=S-1-5-80-446051430-1559341753-4161941529-1950928533-810483104",
		"https://+:443/sra_{BA195980-CD49-458b-9E23-C84EE0ADCD75}/=S-1-5-80-3435701886-799518250-3791383489-3228296122-2938884314,BA,SY",
		"http://+:80/0131501b-d67f-491b-9a40-c4bf27bcb4d4/=NS",
		"https://+:443/C574AC30-5794-4AEE-B1BB-6651C5315029/=NS",
		"http://+:80/116B50EB-ECE2-41ac-8429-9F9E963361B7/=NS",
		"http://+:10246/MDEServer/=AU",
	}
	for forma, texto := range map[string]string{"tal cual": out, "LF": lf} {
		rs, err := ParseURLReservations(texto)
		if err != nil {
			t.Fatalf("%s: %v", forma, err)
		}
		if got := resumenReservas(rs); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n  got  %q\n  want %q", forma, got, want)
		}
		if on := URLReservationsOn(rs, 2869); len(on) != 1 || on[0].URL.Host != "*" {
			t.Errorf("%s: en el 2869 = %+v", forma, on)
		}
		if on := URLReservationsOn(rs, 80); len(on) != 3 {
			t.Errorf("%s: en el 80 hay %d reservas, esperaba 3", forma, len(on))
		}
	}
}

// En inglés, con la forma de la documentación, y en un idioma inventado: se
// lee lo mismo, porque no se lee ninguna palabra. Las entradas que niegan
// ("D") y las mal formadas no son cuentas.
func TestReservasSinIdioma(t *testing.T) {
	en := "\r\nURL Reservations: \r\n----------------- \r\n\r\n" +
		"    Reserved URL            : http://*:2869/ \r\n" +
		"        User: NT AUTHORITY\\LOCAL SERVICE\r\n            Listen: Yes\r\n            Delegate: No\r\n" +
		"            SDDL: D:(A;;GX;;;LS) \r\n\r\n" +
		"    Reserved URL            : http://+:5985/wsman/ \r\n" +
		"        User: NT SERVICE\\WinRM\r\n            Listen: Yes\r\n            Delegate: No\r\n" +
		"            SDDL: D:(A;;GX;;;" + sidWinRM + ")(D;;GX;;;BU)(A;;GX) \r\n\r\n"
	xx := "\nQwe rtyuio: \n------------\n\n" +
		"    Zxc vbn            : https://[::1]:8443:[::1]/api/ \n" +
		"        Mnb: VGT\\Plokij Uhygt\n            Asd: Yes\n" +
		"            Lkj: D:P(A;;GA;;;BA)(A;;GX;;;S-1-5-32-545) \n"
	casos := map[string][]string{
		en: {"http://*:2869/=LS", "http://+:5985/wsman/=" + sidWinRM},
		xx: {"https://[::1]:8443/api/=BA,S-1-5-32-545"},
	}
	for out, want := range casos {
		rs, err := ParseURLReservations(out)
		if err != nil {
			t.Fatal(err)
		}
		if got := resumenReservas(rs); !reflect.DeepEqual(got, want) {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
	// Una lista vacía no es un error; una salida sin la línea de guiones sí.
	if rs, err := ParseURLReservations("\r\nReservas de direcciones URL: \r\n---------------------------- \r\n\r\n\r\n"); err != nil || len(rs) != 0 {
		t.Errorf("vacía: %v, %v", rs, err)
	}
	for _, malo := range []string{"", "El servicio HTTP no está disponible.\r\n", "    Reserved URL : http://*:2869/\r\n"} {
		if rs, err := ParseURLReservations(malo); err == nil {
			t.Errorf("ParseURLReservations(%q) = %v sin error", malo, rs)
		}
	}
}

// Propiedad: con etiquetas al azar en cada línea (otro idioma, cada vez) se
// leen las mismas reservas con las mismas cuentas.
func TestReservasPropiedadSinIdioma(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260928, 1))
	palabra := func() string {
		rs := []rune("abcdefghijklmnñopqrstuvwxyzäöüéè")
		var b strings.Builder
		for n := 2 + rng.IntN(9); n > 0; n-- {
			b.WriteRune(rs[rng.IntN(len(rs))])
		}
		return b.String()
	}
	sids := []string{"LS", "NS", "BU", "WD", "SY", "IU", "AU", "BA", sidWinRM, sidWecsvc}
	for iter := 0; iter < 300; iter++ {
		var b strings.Builder
		fmt.Fprintf(&b, "\r\n%s %s: \r\n%s \r\n\r\n", palabra(), palabra(), strings.Repeat("-", 5+rng.IntN(30)))
		var want []string
		for n := rng.IntN(6); n > 0; n-- {
			url := fmt.Sprintf("http://+:%d/%s/", 1+rng.IntN(65535), palabra())
			fmt.Fprintf(&b, "    %s %s            : %s \r\n", palabra(), palabra(), url)
			var mias []string
			var dacl strings.Builder
			dacl.WriteString("D:")
			for k := 1 + rng.IntN(3); k > 0; k-- {
				s := sids[rng.IntN(len(sids))]
				fmt.Fprintf(&b, "        %s: %s\\%s %s\r\n            %s: Yes\r\n", palabra(), palabra(), palabra(), palabra(), palabra())
				fmt.Fprintf(&dacl, "(A;;GX;;;%s)", s)
				if !slices.Contains(mias, s) {
					mias = append(mias, s)
				}
			}
			fmt.Fprintf(&b, "            %s: %s \r\n\r\n", palabra(), dacl.String())
			u, _ := parseQueueURL(url)
			want = append(want, u.String()+"="+strings.Join(mias, ","))
		}
		rs, err := ParseURLReservations(b.String())
		if err != nil {
			t.Fatalf("iter %d: %v\n%s", iter, err, b.String())
		}
		if got := resumenReservas(rs); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("iter %d:\n  got  %q\n  want %q\n%s", iter, got, want, b.String())
		}
	}
}
