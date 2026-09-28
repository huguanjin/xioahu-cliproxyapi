/**
 * 打包下载必须请求二进制响应。
 *
 * The axios instance is created with a JSON content type and no responseType, so
 * by default it parses every response body as JSON. A zip that arrives as a
 * parsed string cannot be handed to `createObjectURL` — that throws
 * "Overload resolution failed", which is the error the operator saw. The single
 * credential download already passes `responseType: 'blob'`; this asserts the
 * archive path does too, because the two are separate call sites and one of them
 * was missing it.
 */

import { afterEach, describe, expect, test } from 'bun:test';
import { apiClient } from '@/services/api/client';
import { authFilesApi } from '@/services/api/authFiles';

const originalPostRaw = apiClient.postRaw;

afterEach(() => {
  apiClient.postRaw = originalPostRaw;
});

describe('downloadArchive', () => {
  test('requests a blob response', async () => {
    let seenConfig: unknown = null;
    apiClient.postRaw = (async (_url: string, _data?: unknown, config?: unknown) => {
      seenConfig = config;
      return { data: new Blob(['zip']) };
    }) as typeof apiClient.postRaw;

    await authFilesApi.downloadArchive(['a.json']);

    expect(seenConfig).toEqual({ responseType: 'blob' });
  });

  test('posts the names and the email flag', async () => {
    let seenUrl = '';
    let seenBody: unknown = null;
    apiClient.postRaw = (async (url: string, data?: unknown) => {
      seenUrl = url;
      seenBody = data;
      return { data: new Blob(['zip']) };
    }) as typeof apiClient.postRaw;

    await authFilesApi.downloadArchive(['a.json', 'b.json']);

    expect(seenUrl).toBe('/auth-files/download-archive');
    expect(seenBody).toEqual({ names: ['a.json', 'b.json'], include_emails: true });
  });

  test('defaults include_emails to true and honours an explicit false', async () => {
    // 邮箱清单是这个包的主要用途之一，所以缺省必须是开；显式关闭也要生效。
    const seen: unknown[] = [];
    apiClient.postRaw = (async (_url: string, data?: unknown) => {
      seen.push(data);
      return { data: new Blob(['zip']) };
    }) as typeof apiClient.postRaw;

    await authFilesApi.downloadArchive(['a.json']);
    await authFilesApi.downloadArchive(['a.json'], { includeEmails: false });

    expect((seen[0] as { include_emails: boolean }).include_emails).toBe(true);
    expect((seen[1] as { include_emails: boolean }).include_emails).toBe(false);
  });

  test('deduplicates names before sending', async () => {
    let seenBody: unknown = null;
    apiClient.postRaw = (async (_url: string, data?: unknown) => {
      seenBody = data;
      return { data: new Blob(['zip']) };
    }) as typeof apiClient.postRaw;

    await authFilesApi.downloadArchive(['a.json', 'a.json', 'b.json']);

    expect((seenBody as { names: string[] }).names).toEqual(['a.json', 'b.json']);
  });

  test('returns the response data so callers can hand it to downloadBlob', async () => {
    const blob = new Blob(['zip-bytes']);
    apiClient.postRaw = (async () => ({ data: blob })) as typeof apiClient.postRaw;

    const got = await authFilesApi.downloadArchive(['a.json']);
    expect(got).toBe(blob);
    // createObjectURL 只接受 Blob/File/MediaSource —— 传字符串正是当初的报错。
    expect(got instanceof Blob).toBe(true);
  });
});
