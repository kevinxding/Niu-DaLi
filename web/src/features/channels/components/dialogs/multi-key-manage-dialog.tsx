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
import { useQueryClient } from '@tanstack/react-query'
import { Loader2, RefreshCw, Trash2, Power, PowerOff } from 'lucide-react'
import { useState, useEffect } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatQuotaWithCurrency } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import {
  getMultiKeyStatus,
  enableMultiKey,
  disableMultiKey,
  deleteMultiKey,
  enableAllMultiKeys,
  disableAllMultiKeys,
  deleteDisabledMultiKeys,
} from '../../api'
import {
  MULTI_KEY_FILTER_OPTIONS,
  MULTI_KEY_MODES,
  SENSITIVE_MASK,
} from '../../constants'
import {
  channelsQueryKeys,
  formatResponseTime,
  formatTimestamp,
  getMultiKeyStatusConfig,
  getMultiKeyConfirmMessage,
  getResponseTimeConfig,
  handleTestChannel,
  isDestructiveAction,
} from '../../lib'
import type { KeyStatus, MultiKeyConfirmAction } from '../../types'
import { useChannels } from '../channels-provider'
import { StatisticsCard } from './multi-key-statistics-card'
import { MultiKeyTableRowActions } from './multi-key-table-row-actions'

type MultiKeyManageDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
}

type KeyTestResult = {
  success: boolean
  responseTime?: number
  message?: string
}

// Test state is keyed by channel and key so a test that finishes after the
// dialog switched channels cannot land on another channel's row.
function keyTestId(channelId: number, keyIndex: number) {
  return `${channelId}:${keyIndex}`
}

export function MultiKeyManageDialog({
  open,
  onOpenChange,
}: MultiKeyManageDialogProps) {
  const { t } = useTranslation()
  const { currentRow, sensitiveVisible } = useChannels()
  const queryClient = useQueryClient()
  const currentUser = useAuthStore((s) => s.auth.user)
  const canEditSensitive = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.SENSITIVE_WRITE
  )

  // Data state
  const [isLoading, setIsLoading] = useState(false)
  const [keys, setKeys] = useState<KeyStatus[]>([])
  const [currentPage, setCurrentPage] = useState(1)
  const [pageSize, setPageSize] = useState(10)
  const [total, setTotal] = useState(0)
  const [totalPages, setTotalPages] = useState(0)
  const [enabledCount, setEnabledCount] = useState(0)
  const [manualDisabledCount, setManualDisabledCount] = useState(0)
  const [autoDisabledCount, setAutoDisabledCount] = useState(0)
  const [balanceFailureThreshold, setBalanceFailureThreshold] = useState(0)

  // UI state
  const [statusFilter, setStatusFilter] = useState<number | null>(null)
  const [confirmAction, setConfirmAction] =
    useState<MultiKeyConfirmAction | null>(null)
  const [isPerformingAction, setIsPerformingAction] = useState(false)
  const [testingKeys, setTestingKeys] = useState<ReadonlySet<string>>(
    () => new Set()
  )
  const [testResults, setTestResults] = useState<Record<string, KeyTestResult>>(
    {}
  )

  // Reset and load data when dialog opens
  useEffect(() => {
    if (open && currentRow) {
      setCurrentPage(1)
      setStatusFilter(null)
      setTestResults({})
      loadKeyStatus(1, pageSize, null)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, currentRow?.id])

  const loadKeyStatus = async (
    page: number = currentPage,
    size: number = pageSize,
    status: number | null = statusFilter
  ) => {
    if (!currentRow) return

    setIsLoading(true)
    try {
      const response = await getMultiKeyStatus(
        currentRow.id,
        page,
        size,
        status === null ? undefined : status
      )

      if (response.success && response.data) {
        setKeys(response.data.keys || [])
        setTotal(response.data.total || 0)
        setCurrentPage(response.data.page || 1)
        setPageSize(response.data.page_size || 10)
        setTotalPages(response.data.total_pages || 0)
        setEnabledCount(response.data.enabled_count || 0)
        setManualDisabledCount(response.data.manual_disabled_count || 0)
        setAutoDisabledCount(response.data.auto_disabled_count || 0)
        setBalanceFailureThreshold(response.data.balance_failure_threshold || 0)
      } else {
        handleServerError(response, t('Failed to load key status'))
      }
    } catch (error: unknown) {
      handleServerError(error, t('Failed to load key status'))
    } finally {
      setIsLoading(false)
    }
  }

  const handleStatusFilterChange = (value: string) => {
    const newFilter = value === 'all' ? null : Number.parseInt(value)
    setStatusFilter(newFilter)
    setCurrentPage(1)
    loadKeyStatus(1, pageSize, newFilter)
  }

  const handlePageChange = (newPage: number) => {
    setCurrentPage(newPage)
    loadKeyStatus(newPage, pageSize)
  }

  const performAction = async () => {
    if (!confirmAction || !currentRow) return
    if (
      !canEditSensitive &&
      (confirmAction.type === 'delete' ||
        confirmAction.type === 'delete-disabled')
    ) {
      setConfirmAction(null)
      return
    }

    setIsPerformingAction(true)
    try {
      const { type, keyIndex } = confirmAction
      let response

      // Execute the appropriate action
      if (type === 'enable' && keyIndex !== undefined) {
        response = await enableMultiKey(currentRow.id, keyIndex)
      } else if (type === 'disable' && keyIndex !== undefined) {
        response = await disableMultiKey(currentRow.id, keyIndex)
      } else if (type === 'delete' && keyIndex !== undefined) {
        response = await deleteMultiKey(currentRow.id, keyIndex)
      } else if (type === 'enable-all') {
        response = await enableAllMultiKeys(currentRow.id)
      } else if (type === 'disable-all') {
        response = await disableAllMultiKeys(currentRow.id)
      } else if (type === 'delete-disabled') {
        response = await deleteDisabledMultiKeys(currentRow.id)
      }

      if (response?.success) {
        toast.success(response.message || t('Operation successful'))
        queryClient.invalidateQueries({ queryKey: channelsQueryKeys.lists() })

        // Reload data - reset to page 1 for bulk actions
        const isBulkAction = type.includes('all') || type === 'delete-disabled'
        if (isBulkAction) {
          setCurrentPage(1)
          loadKeyStatus(1, pageSize)
        } else {
          loadKeyStatus(currentPage, pageSize)
        }
      } else {
        handleServerError(response, t('Operation failed'))
      }
    } catch (error: unknown) {
      handleServerError(error, t('Operation failed'))
    } finally {
      setIsPerformingAction(false)
      setConfirmAction(null)
    }
  }

  const handleTestKey = async (keyIndex: number) => {
    if (!currentRow) return
    const testId = keyTestId(currentRow.id, keyIndex)
    setTestingKeys((prev) => new Set(prev).add(testId))
    await handleTestChannel(
      currentRow.id,
      { keyIndex },
      (success, responseTime, error) => {
        setTestResults((prev) => ({
          ...prev,
          [testId]: { success, responseTime, message: error },
        }))
      }
    )
    setTestingKeys((prev) => {
      const next = new Set(prev)
      next.delete(testId)
      return next
    })
  }

  const renderTestResult = (keyIndex: number) => {
    if (!currentRow) return null
    const result = testResults[keyTestId(currentRow.id, keyIndex)]
    if (!result) return null
    if (!result.success) {
      return (
        <StatusBadge
          label={t('Test failed')}
          variant='danger'
          copyable={false}
          title={result.message}
        />
      )
    }
    const responseTime = result.responseTime ?? 0
    return (
      <StatusBadge
        label={
          responseTime > 0
            ? t('Test passed · {{duration}}', {
                duration: formatResponseTime(responseTime, t),
              })
            : t('Test passed')
        }
        variant={getResponseTimeConfig(responseTime).variant}
        copyable={false}
      />
    )
  }

  const renderStatusBadge = (status: number) => {
    const config = getMultiKeyStatusConfig(status)
    return (
      <StatusBadge
        label={t(config.label)}
        variant={config.variant}
        showDot
        copyable={false}
      />
    )
  }

  const formatKeyTimestamp = (timestamp?: number) => {
    if (!timestamp) return '-'
    return formatTimestamp(timestamp)
  }

  if (!currentRow) return null

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={onOpenChange}
        title={
          <>
            {t('Multi-Key Management')}
            <StatusBadge
              label={currentRow.name}
              variant='neutral'
              copyable={false}
            />
            {currentRow.channel_info?.multi_key_mode && (
              <StatusBadge
                label={t(
                  MULTI_KEY_MODES.find(
                    (option) =>
                      option.value === currentRow.channel_info.multi_key_mode
                  )?.label ?? 'Random'
                )}
                variant='neutral'
                copyable={false}
              />
            )}
          </>
        }
        description={t(
          'Manage multi-key status and configuration for this channel'
        )}
        contentClassName='flex max-h-[min(90dvh,var(--dialog-available-height))] max-w-5xl flex-col'
        titleClassName='flex items-center gap-2'
        contentHeight='min(72vh, 720px)'
        bodyClassName='space-y-4'
      >
        <div className='flex min-h-0 flex-1 flex-col space-y-4 overflow-hidden'>
          {/* Statistics */}
          <div className='grid shrink-0 grid-cols-3 gap-3'>
            <StatisticsCard
              label={t('Enabled')}
              count={enabledCount}
              total={total}
            />
            <StatisticsCard
              label={t('Manual Disabled')}
              count={manualDisabledCount}
              total={total}
            />
            <StatisticsCard
              label={t('Auto Disabled')}
              count={autoDisabledCount}
              total={total}
            />
          </div>

          <Separator className='shrink-0' />

          {/* Toolbar */}
          <div className='flex shrink-0 items-center justify-between'>
            <Select
              items={MULTI_KEY_FILTER_OPTIONS.map((option) => ({
                value: option.value,
                label: t(option.label),
              }))}
              value={statusFilter === null ? 'all' : statusFilter.toString()}
              onValueChange={(v) => v !== null && handleStatusFilterChange(v)}
            >
              <SelectTrigger className='w-40'>
                <SelectValue placeholder={t('All Status')} />
              </SelectTrigger>
              <SelectContent alignItemWithTrigger={false}>
                <SelectGroup>
                  {MULTI_KEY_FILTER_OPTIONS.map((option) => (
                    <SelectItem key={option.value} value={option.value}>
                      {t(option.label)}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>

            <div className='flex items-center gap-2'>
              <Button
                variant='outline'
                size='sm'
                onClick={() => loadKeyStatus()}
                disabled={isLoading}
              >
                <RefreshCw className='h-4 w-4' />
              </Button>

              {manualDisabledCount + autoDisabledCount > 0 && (
                <Button
                  variant='default'
                  size='sm'
                  onClick={() => setConfirmAction({ type: 'enable-all' })}
                >
                  <Power className='mr-2 h-4 w-4' />
                  {t('Enable All')}
                </Button>
              )}

              {enabledCount > 0 && (
                <Button
                  variant='destructive'
                  size='sm'
                  onClick={() => setConfirmAction({ type: 'disable-all' })}
                >
                  <PowerOff className='mr-2 h-4 w-4' />
                  {t('Disable All')}
                </Button>
              )}

              {autoDisabledCount > 0 && (
                <Button
                  variant='destructive'
                  size='sm'
                  onClick={() => {
                    if (!canEditSensitive) return
                    setConfirmAction({ type: 'delete-disabled' })
                  }}
                  disabled={!canEditSensitive}
                  title={
                    canEditSensitive
                      ? undefined
                      : t('No permission to perform this action')
                  }
                >
                  <Trash2 className='mr-2 h-4 w-4' />
                  {t('Delete Auto-Disabled')}
                </Button>
              )}
            </div>
          </div>
          {!canEditSensitive && (
            <p className='text-muted-foreground text-xs'>
              {t('No permission to perform this action')}
            </p>
          )}

          {/* Table */}
          <div className='min-h-0 flex-1 overflow-auto rounded-md border'>
            {isLoading && (
              <div className='flex items-center justify-center py-12'>
                <Loader2 className='text-muted-foreground h-8 w-8 animate-spin' />
              </div>
            )}
            {!isLoading && keys.length === 0 && (
              <div className='text-muted-foreground py-12 text-center'>
                {t('No keys found')}
              </div>
            )}
            {!isLoading && !(keys.length === 0) && (
              <StaticDataTable
                // Pinned cells must be opaque; match the dialog surface.
                className='rounded-none border-0 [--table-header-bg:var(--popover)] [--table-pinned-bg:var(--popover)]'
                tableClassName='min-w-[800px]'
                data={keys}
                getRowKey={(key) => key.index}
                columns={[
                  {
                    id: 'index',
                    header: t('Index'),
                    className: 'w-20',
                    cellClassName: 'font-mono text-sm',
                    cell: (key) => `#${key.index + 1}`,
                  },
                  {
                    id: 'status',
                    header: t('Status'),
                    className: 'w-36',
                    cell: (key) => (
                      <div className='flex flex-col items-start gap-1'>
                        {renderStatusBadge(key.status)}
                        {renderTestResult(key.index)}
                        {key.status === 1 &&
                          (key.balance_failures ?? 0) > 0 && (
                            <span className='text-warning text-xs'>
                              {t('Balance failures: {{count}}/{{threshold}}', {
                                count: key.balance_failures,
                                threshold: balanceFailureThreshold,
                              })}
                            </span>
                          )}
                      </div>
                    ),
                  },
                  {
                    id: 'used-quota',
                    header: t('Used Quota'),
                    className: 'w-32',
                    cellClassName: 'font-mono text-sm',
                    cell: (key) =>
                      sensitiveVisible
                        ? formatQuotaWithCurrency(key.used_quota ?? 0, {
                            digitsLarge: 2,
                            digitsSmall: 4,
                          })
                        : SENSITIVE_MASK,
                  },
                  {
                    id: 'reason',
                    header: t('Disabled Reason'),
                    className: 'min-w-[200px]',
                    cellClassName: 'max-w-xs truncate text-sm',
                    cell: (key) => key.reason || '-',
                  },
                  {
                    id: 'disabled-time',
                    header: t('Disabled Time'),
                    className: 'w-44',
                    cellClassName: 'text-muted-foreground text-sm',
                    cell: (key) => formatKeyTimestamp(key.disabled_time),
                  },
                  {
                    id: 'actions',
                    header: t('Actions'),
                    className: 'text-right',
                    // The table is wider than the dialog on most screens.
                    pinned: 'right',
                    cell: (key) => (
                      <MultiKeyTableRowActions
                        keyIndex={key.index}
                        status={key.status}
                        canDelete={canEditSensitive}
                        isTesting={testingKeys.has(
                          keyTestId(currentRow.id, key.index)
                        )}
                        onAction={setConfirmAction}
                        onTest={() => handleTestKey(key.index)}
                      />
                    ),
                  },
                ]}
              />
            )}
          </div>

          {/* Pagination */}
          {totalPages > 1 && (
            <div className='flex shrink-0 items-center justify-between'>
              <div className='text-muted-foreground text-sm'>
                {t('Page {{current}} of {{total}}', {
                  current: currentPage,
                  total: totalPages,
                })}
              </div>
              <div className='flex gap-2'>
                <Button
                  variant='outline'
                  size='sm'
                  onClick={() => handlePageChange(currentPage - 1)}
                  disabled={currentPage === 1 || isLoading}
                >
                  {t('Previous')}
                </Button>
                <Button
                  variant='outline'
                  size='sm'
                  onClick={() => handlePageChange(currentPage + 1)}
                  disabled={currentPage >= totalPages || isLoading}
                >
                  {t('Next')}
                </Button>
              </div>
            </div>
          )}
        </div>
      </Dialog>

      {/* Confirmation Dialog */}
      <ConfirmDialog
        open={confirmAction !== null}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t('Confirm Action')}
        desc={t(getMultiKeyConfirmMessage(confirmAction))}
        destructive={isDestructiveAction(confirmAction)}
        isLoading={isPerformingAction}
        handleConfirm={performAction}
      />
    </>
  )
}
