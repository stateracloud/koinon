// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package guestfront_test

func subreaperFlag() bool { return false }

func setSubreaper(bool) {}
