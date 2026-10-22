// SPDX-License-Identifier: Apache-2.0

import { ConnectionStatus } from '@/gen/koinon/connection/v1alpha1/connection_pb'

export function connectionStatusLabel(s: ConnectionStatus): string {
  switch (s) {
    case ConnectionStatus.OK:
      return 'Working'
    case ConnectionStatus.FAILING:
      return 'Failing'
    case ConnectionStatus.UNTESTED:
      return 'Not tested'
    default:
      return 'Unknown'
  }
}
