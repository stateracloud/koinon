// SPDX-License-Identifier: Apache-2.0

/** downloadText gives the browser a text file to save, such as a CSV export. */
export function downloadText(filename: string, text: string, type = 'text/csv;charset=utf-8') {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  document.body.append(a)
  a.click()
  a.remove()
  // The click starts the download at once; free the URL after this task.
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
