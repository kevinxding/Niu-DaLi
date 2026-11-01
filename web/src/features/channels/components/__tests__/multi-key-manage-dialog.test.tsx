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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import { useEffect } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import { channelSchema, type KeyStatus } from '../../types'
import { ChannelsProvider, useChannels } from '../channels-provider'
import { MultiKeyManageDialog } from '../dialogs/multi-key-manage-dialog'

let client: QueryClient

const multiKeyChannel = channelSchema.parse({
  id: 7,
  name: 'Pooled channel',
  type: 1,
  key: '',
  status: 1,
  created_time: 1,
  test_time: 0,
  response_time: 0,
  balance_updated_time: 0,
  models: 'gpt-4o',
  group: 'default',
  channel_info: {
    is_multi_key: true,
    multi_key_size: 2,
    multi_key_mode: 'sequential',
  },
})

const keys: KeyStatus[] = [
  {
    index: 0,
    status: 1,
    key_preview: 'sk-first...',
    used_quota: 1_500_000,
    balance_failures: 2,
  },
  {
    index: 1,
    status: 3,
    key_preview: 'sk-second...',
    reason: 'Insufficient Balance',
    used_quota: 250_000,
    balance_failures: 0,
  },
]

function DialogHarness(props: { sensitiveVisible: boolean }) {
  const { currentRow, setCurrentRow, setSensitiveVisible } = useChannels()
  useEffect(() => {
    setCurrentRow(multiKeyChannel)
    setSensitiveVisible(props.sensitiveVisible)
  }, [setCurrentRow, setSensitiveVisible, props.sensitiveVisible])
  if (!currentRow) return null
  return <MultiKeyManageDialog open onOpenChange={() => undefined} />
}

function renderDialog(sensitiveVisible: boolean) {
  render(
    <QueryClientProvider client={client}>
      <ChannelsProvider>
        <DialogHarness sensitiveVisible={sensitiveVisible} />
      </ChannelsProvider>
    </QueryClientProvider>
  )
}

beforeEach(() => {
  useSystemConfigStore.getState().setConfig({
    currency: {
      ...DEFAULT_CURRENCY_CONFIG,
      quotaDisplayType: 'USD',
      quotaPerUnit: 500_000,
    },
  })
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        keys,
        total: keys.length,
        page: 1,
        page_size: 10,
        total_pages: 1,
        enabled_count: 1,
        manual_disabled_count: 0,
        auto_disabled_count: 1,
        balance_failure_threshold: 5,
      },
    },
  })
})

afterEach(() => {
  vi.restoreAllMocks()
  client.clear()
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
})

test('key manager shows each key used quota and the balance failure streak of enabled keys', async () => {
  renderDialog(true)

  const firstRow = await screen.findByRole('row', { name: /^#1 / })
  const secondRow = screen.getByRole('row', { name: /^#2 / })
  // 1,500,000 and 250,000 quota at 500,000 quota per USD.
  expect(within(firstRow).getByText('$3')).toBeVisible()
  expect(within(firstRow).getByText('Balance failures: 2/5')).toBeVisible()
  expect(within(secondRow).getByText('$0.5')).toBeVisible()
  expect(
    within(secondRow).queryByText(/Balance failures/)
  ).not.toBeInTheDocument()
  expect(api.post).toHaveBeenCalledWith(
    '/api/channel/multi_key/manage',
    expect.objectContaining({ channel_id: 7, action: 'get_key_status' }),
    expect.anything()
  )
})

test('key manager masks per-key used quota while sensitive information is hidden', async () => {
  renderDialog(false)

  const firstRow = await screen.findByRole('row', { name: /^#1 / })
  expect(within(firstRow).getByText('••••')).toBeVisible()
  expect(screen.queryByText('$3')).not.toBeInTheDocument()
})
