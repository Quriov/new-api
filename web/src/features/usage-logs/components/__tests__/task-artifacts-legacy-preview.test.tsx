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
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, test, vi } from 'vitest'

import type { TaskLog } from '../../types'
import { TaskArtifactsCell } from '../task-artifacts'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))

vi.mock('@/lib/api', () => ({
  api: { get },
}))

const legacyContentUrl = `https://gateway.example/v1/tasks/task-legacy/artifacts/video/content?access=${'A'.repeat(41)}-_`

// A non-plugin task: image models are submitted through the video endpoint,
// so the list only says a legacy result is available.
function legacyTaskLog(): TaskLog {
  return {
    id: 1,
    user_id: 7,
    platform: 'openai-video',
    task_id: 'task-legacy',
    action: 'textGenerate',
    channel_id: 12,
    group: 'default',
    quota: 25000,
    submit_time: 1,
    status: 'SUCCESS',
    legacy_video_available: true,
  }
}

function mockArtifactsResponse(data: Record<string, unknown>) {
  get.mockResolvedValue({ data: { success: true, data } })
}

async function openLegacyPreview() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={queryClient}>
      <TaskArtifactsCell log={legacyTaskLog()} />
    </QueryClientProvider>
  )
  await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
  return screen.findByRole('dialog')
}

describe('legacy task result preview', () => {
  beforeEach(() => {
    get.mockReset()
  })

  test('given a legacy result the server detected as an image, the dialog previews an image', async () => {
    mockArtifactsResponse({
      artifacts: [],
      legacy_content_url: legacyContentUrl,
      legacy_content_type: 'image',
      legacy_mime_type: 'image/png',
    })

    const dialog = await openLegacyPreview()

    const image = await within(dialog).findByRole('img', { name: 'image' })
    expect(image).toHaveAttribute('src', legacyContentUrl)
    expect(dialog.querySelector('video')).toBeNull()
    expect(within(dialog).getByText('Image')).toBeVisible()
    expect(within(dialog).queryByText('Video')).not.toBeInTheDocument()
    expect(within(dialog).getByText('image/png')).toBeVisible()
    expect(
      within(dialog).getByRole('button', { name: /Download/ })
    ).toHaveAttribute('href', legacyContentUrl)
    expect(get).toHaveBeenCalledWith(
      '/api/task/task-legacy/artifacts',
      expect.anything()
    )
  })

  test('given a legacy result the server detected as a video, the dialog still previews a video', async () => {
    mockArtifactsResponse({
      artifacts: [],
      legacy_content_url: legacyContentUrl,
      legacy_content_type: 'video',
      legacy_mime_type: 'video/mp4',
    })

    const dialog = await openLegacyPreview()

    await within(dialog).findByText('Video')
    const video = dialog.querySelector('video')
    expect(video).not.toBeNull()
    expect(video).toHaveAttribute('src', legacyContentUrl)
    expect(dialog.querySelector('img')).toBeNull()
    expect(
      within(dialog).getByRole('button', { name: /Download/ })
    ).toHaveAttribute('href', legacyContentUrl)
  })

  test('given a server that does not report the legacy type, the dialog keeps the video preview', async () => {
    mockArtifactsResponse({
      artifacts: [],
      legacy_content_url: legacyContentUrl,
    })

    const dialog = await openLegacyPreview()

    await within(dialog).findByText('Video')
    expect(dialog.querySelector('video')).toHaveAttribute(
      'src',
      legacyContentUrl
    )
    expect(dialog.querySelector('img')).toBeNull()
  })
})
