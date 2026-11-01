/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { describe, expect, test } from 'vitest'

import { CHANNEL_FORM_DEFAULT_VALUES, channelFormSchema } from '../channel-form'

const VERTEX_KEY_ERROR = 'Vertex AI service account key must be valid JSON'

// The private key holds escaped newlines and braces that must not split keys.
const keyA =
  '{"type":"service_account","project_id":"a","private_key":"-----BEGIN PRIVATE KEY-----\\nab{c}\\"d\\n-----END PRIVATE KEY-----\\n"}'
const keyB = '{"type":"service_account","project_id":"b"}'
const prettyB = '{\n  "type": "service_account",\n  "project_id": "b"\n}'

function vertexForm(key: string) {
  return {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'Vertex pool',
    type: 41,
    vertex_key_type: 'json' as const,
    multi_key_mode: 'multi_to_single' as const,
    other: 'us-central1',
    key,
    models: 'gemini-2.5-pro',
  }
}

function hasVertexKeyError(key: string) {
  const result = channelFormSchema.safeParse(vertexForm(key))
  return (
    !result.success &&
    result.error.issues.some(
      (issue) => issue.path[0] === 'key' && issue.message === VERTEX_KEY_ERROR
    )
  )
}

describe('Vertex AI service account keys', () => {
  test.each([
    ['a JSON array', `[${keyA},${keyB}]`],
    ['a single object', keyA],
    ['one object per line', `${keyA}\n${keyB}\n`],
    ['pretty-printed objects back to back', `${keyA}\n\n${prettyB}`],
    ['comma-separated objects', `${keyA},\n${keyB}`],
  ])('accepts %s', (_shape, key) => {
    expect(hasVertexKeyError(key)).toBe(false)
  })

  test.each([
    ['text after an object', `${keyA}\nnot-json`],
    ['an unclosed object', '{"type":"service_account"'],
    ['an invalid object', '{"type": service_account}'],
    ['an array of strings', '["key-a"]'],
  ])('rejects %s', (_shape, key) => {
    expect(hasVertexKeyError(key)).toBe(true)
  })
})
