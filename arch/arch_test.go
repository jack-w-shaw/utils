// Copyright 2014 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package arch_test

import (
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"

	"github.com/juju/utils/v3/arch"
)

type archSuite struct {
}

var _ = gc.Suite(&archSuite{})

func (s *archSuite) TestHostArch(c *gc.C) {
	a := arch.HostArch()
	c.Assert(arch.IsSupportedArch(a), jc.IsTrue)
}

func (s *archSuite) TestNormaliseArch(c *gc.C) {
	for _, test := range []struct {
		raw  string
		arch string
	}{
		{"windows", "windows"},
		{"amd64", "amd64"},
		{"x86_64", "amd64"},
		{"386", "i386"},
		{"i386", "i386"},
		{"i486", "i386"},
		{"arm", "armhf"},
		{"armv", "armhf"},
		{"armv7", "armhf"},
		{"aarch64", "arm64"},
		{"arm64", "arm64"},
		{"ppc64el", "ppc64el"},
		{"ppc64le", "ppc64el"},
		{"ppc64", "ppc64el"},
		{"s390x", "s390x"},
		{"riscv64", "riscv64"},
		{"risc", "riscv64"},
		{"risc-v64", "riscv64"},
		{"risc-V64", "riscv64"},
	} {
		arch := arch.NormaliseArch(test.raw)
		c.Check(arch, gc.Equals, test.arch)
	}
}

func (s *archSuite) TestIsSupportedArch(c *gc.C) {
	for _, a := range arch.AllSupportedArches {
		c.Assert(arch.IsSupportedArch(a), jc.IsTrue)
	}
	c.Assert(arch.IsSupportedArch("invalid"), jc.IsFalse)
}

func (s *archSuite) TestArchInfo(c *gc.C) {
	for _, a := range arch.AllSupportedArches {
		_, ok := arch.Info[a]
		c.Assert(ok, jc.IsTrue)
	}
}
