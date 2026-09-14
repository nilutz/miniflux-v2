// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !ORT

package onnx // import "miniflux.app/v2/sidecar/internal/embed/onnx"

// BackendName reports which inference backend this binary was compiled with.
//
// Reaching this file means the ORT build tag was omitted, which silently
// selects the pure-Go GoMLX backend — roughly an order of magnitude slower
// (spec §6.7). TestBackendIsORT fails loudly rather than letting that reach
// production.
func BackendName() string { return "GoMLX" }
