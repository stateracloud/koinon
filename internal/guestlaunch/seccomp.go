// SPDX-License-Identifier: Apache-2.0

package guestlaunch

import (
	"fmt"
	"sort"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/net/bpf"
)

// cloneNamespaceFlags are the clone(2) flags that create a namespace: NEWNS, NEWCGROUP, NEWUTS,
// NEWIPC, NEWUSER, NEWPID, NEWNET and NEWTIME.
const cloneNamespaceFlags uint64 = 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 |
	0x10000000 | 0x20000000 | 0x40000000 | 0x00000080

// denyList returns the sorted deny list the policy asks for.
func (s SeccompPolicy) denyList() []string {
	base := DefaultSeccompDeny
	if len(s.Deny) > 0 {
		base = s.Deny
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range append(append([]string{}, base...), s.DenyExtra...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// BuildSeccomp assembles the deny filter for the running architecture (go-seccomp-bpf assembles
// for the CPU it runs on). It returns the filter and the names the architecture does not have
// (skipped, for the log).
//
// The filter allows everything except the deny list (each denied call returns EPERM), denies
// clone(2) with a namespace flag, and makes clone3 return ENOSYS: clone3 passes its flags in a
// struct that seccomp cannot read, and ENOSYS is what makes glibc fall back to clone (the way
// Docker's default profile does it).
func BuildSeccomp(s SeccompPolicy) (filter []bpf.Instruction, skipped []string, err error) {
	info, err := arch.GetInfo("")
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, n := range s.denyList() {
		if _, ok := info.SyscallNames[n]; ok {
			names = append(names, n)
		} else {
			skipped = append(skipped, n)
		}
	}
	groups := []seccomp.SyscallGroup{{Names: names, Action: seccomp.ActionErrno}}
	if _, ok := info.SyscallNames["clone"]; ok {
		groups = append(groups, seccomp.SyscallGroup{
			NamesWithCondtions: []seccomp.NameWithConditions{{
				Name:       "clone",
				Conditions: seccomp.ArgumentConditions{{Argument: 0, Operation: seccomp.BitsSet, Value: cloneNamespaceFlags}},
			}},
			Action: seccomp.ActionErrno,
		})
	}
	pol := seccomp.Policy{DefaultAction: seccomp.ActionAllow, Syscalls: groups}
	insts, err := pol.Assemble()
	if err != nil {
		return nil, nil, fmt.Errorf("assemble seccomp policy: %w", err)
	}
	if clone3, ok := info.SyscallNames["clone3"]; ok {
		insts = append(clone3ENOSYS(info, uint32(clone3)), insts...)
	}
	return insts, skipped, nil
}

// clone3ENOSYS is a self-contained prefix: on the native architecture a clone3 call returns ENOSYS,
// everything else falls through to the next instruction.
func clone3ENOSYS(info *arch.Info, nr uint32) []bpf.Instruction {
	const enosys = 38
	return []bpf.Instruction{
		bpf.LoadAbsolute{Off: 4, Size: 4}, // seccomp_data.arch
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(info.ID), SkipTrue: 3},
		bpf.LoadAbsolute{Off: 0, Size: 4}, // seccomp_data.nr
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: nr, SkipTrue: 1},
		bpf.RetConstant{Val: uint32(seccomp.ActionErrno) | enosys},
	}
}
