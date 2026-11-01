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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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

test('testing a key sends that key index and shows the result on its row only', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, message: '', time: 1.234 },
  })
  const user = userEvent.setup()
  renderDialog(true)

  const secondRow = await screen.findByRole('row', { name: /^#2 / })
  await user.click(within(secondRow).getByRole('button', { name: 'Test' }))

  expect(api.get).toHaveBeenCalledWith(
    '/api/channel/test/7',
    expect.objectContaining({ params: { key_index: 1 } })
  )
  expect(
    await within(secondRow).findByText('Test passed · 1.23s')
  ).toBeVisible()
  const firstRow = screen.getByRole('row', { name: /^#1 / })
  expect(within(firstRow).queryByText(/Test passed/)).not.toBeInTheDocument()
})

test('a failed key test marks the row and keeps the upstream error as its title', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: false,
      message: 'invalid_grant: account not found',
      time: 0.4,
    },
  })
  const user = userEvent.setup()
  renderDialog(true)

  const firstRow = await screen.findByRole('row', { name: /^#1 / })
  await user.click(within(firstRow).getByRole('button', { name: 'Test' }))

  const failure = await within(firstRow).findByText('Test failed')
  expect(failure.closest('[data-slot="status-badge"]')).toHaveAttribute(
    'title',
    'invalid_grant: account not found'
  )
  await waitFor(() =>
    expect(within(firstRow).getByRole('button', { name: 'Test' })).toBeEnabled()
  )
})

test('disabling one enabled key after confirming sends disable_key for that index', async () => {
  const user = userEvent.setup()
  renderDialog(true)

  const firstRow = await screen.findByRole('row', { name: /^#1 / })
  const secondRow = screen.getByRole('row', { name: /^#2 / })
  expect(
    within(secondRow).getByRole('button', { name: 'Enable' })
  ).toBeVisible()
  await user.click(within(firstRow).getByRole('button', { name: 'Disable' }))
  await user.click(await screen.findByRole('button', { name: 'Continue' }))

  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith(
      '/api/channel/multi_key/manage',
      { channel_id: 7, action: 'disable_key', key_index: 0 },
      expect.anything()
    )
  )
})

test('the actions column stays pinned to the right edge while the table scrolls', async () => {
  renderDialog(true)

  const firstRow = await screen.findByRole('row', { name: /^#1 / })
  const header = screen.getByRole('columnheader', { name: 'Actions' })
  const actionsCell = within(firstRow)
    .getByRole('button', { name: 'Test' })
    .closest('td')
  expect(header).toHaveClass('sticky', 'right-0')
  expect(actionsCell).toHaveClass('sticky', 'right-0')
})
