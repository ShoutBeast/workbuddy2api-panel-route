package upstream

import "testing"

// 品牌词改写保持上游**全局替换**语义，只额外放行 .exe 文件名：
// 命中点后紧跟 .exe（大小写不敏感）时原样保留，两种书写形态都不动。
func TestBrandReplaceExeAllowed(t *testing.T) {
	spaced, joined := brandSpacedHex, brandJoinedHex
	cases := []struct{ name, in, want string }{
		{"带空格形态 + .exe", "run " + spaced + ".exe --help", "run " + spaced + ".exe --help"},
		{"连写形态 + .exe", "run " + joined + ".exe --force", "run " + joined + ".exe --force"},
		{"大写扩展名", "run " + spaced + ".EXE", "run " + spaced + ".EXE"},
		{"Windows 路径中的 exe", `C:\tools\` + spaced + `.exe`, `C:\tools\` + spaced + `.exe`},
		{"普通出现仍改写（上游语义）", "powered by " + spaced + ".", "powered by " + joined + "."},
		{"同一串里 exe 与普通出现并存", spaced + ".exe and " + spaced + " agent", spaced + ".exe and " + joined + " agent"},
		{"exe 之后继续改写", spaced + ".exe " + spaced, spaced + ".exe " + joined},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeText(c.in); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// 预检仍要命中品牌词（上游行为）：只有品牌词、没有其它指纹时也必须进入净化流程。
// 同时这条钉住「特征表里的品牌词常量」与替换表用的是同一个字节形态。
func TestBrandPrecheckStillHits(t *testing.T) {
	if !hasFingerprint("prefix " + brandSpacedHex + " suffix") {
		t.Error("brand word should hit the fast path")
	}
}

// 常量自证：两种形态必须是预期的字节序列（防止字面量被改写后静默失效）。
func TestBrandConstantsExact(t *testing.T) {
	if len([]byte(brandSpacedHex)) != 16 {
		t.Fatalf("带空格形态应为 16 字节，实际 %d", len([]byte(brandSpacedHex)))
	}
	if len([]byte(brandJoinedHex)) != 15 {
		t.Fatalf("连写形态应为 15 字节，实际 %d", len([]byte(brandJoinedHex)))
	}
	if brandSpacedHex[:8] != brandJoinedHex[:8] {
		t.Fatal("两种形态应共享前 8 字节（品牌前缀）")
	}
}
