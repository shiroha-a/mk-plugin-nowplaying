/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/*
 * バックエンド呼び出しの薄いラッパ。
 *
 * host.api は POST 固定 (misskeyApi と同じ) なので、バックエンド側も POST で
 * 揃えてある。ジャケット画像だけは <img> が GET しか出せないため GET。
 */

let call: <T>(endpoint: string, params?: Record<string, unknown>) => Promise<T>;

/** Called once from the plugin's setup. */
export function initApi(fn: typeof call): void {
	call = fn;
}

export function api<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
	return call<T>(`plugin/nowplaying/${path}`, params);
}

export type MeResponse = {
	service: string | null;
	username: string | null;
	/** インスタンスに Last.fm の API キーが設定されているか。 */
	lastFmAvailable: boolean;
};

export type Track = {
	title: string;
	artist: string;
	album?: string;
	/** 中継済みのジャケット URL。引けなかった場合は無い。 */
	art?: string;
	/** 再生時刻 (unix 秒)。再生中のものには無い。 */
	listenedAt?: number;
	/** 取得元でのページ。 */
	url?: string;
};

export type LinkedProfile = {
	linked: true;
	service: string;
	username: string;
	playing: Track | null;
	recent: Track[];
	fetchedAt: string;
};

export type ProfileResponse = { linked: false } | LinkedProfile;

export const SERVICES = ['listenbrainz', 'lastfm'] as const;

const SERVICE_LABELS: Record<string, string> = {
	listenbrainz: 'ListenBrainz',
	lastfm: 'Last.fm',
};

export function serviceLabel(service: string): string {
	return SERVICE_LABELS[service] ?? service;
}

/**
 * 相対時刻。プラグインからは MkTime を使えないので自前で組む。
 *
 * 「いつ聴いたか」が分かればよいので、粒度は粗くてよい。
 */
export function relativeTime(unix?: number): string {
	if (unix == null || unix === 0) return '';
	const diff = Date.now() / 1000 - unix;
	if (diff < 0) return '';
	if (diff < 60) return 'たった今';
	if (diff < 3600) return `${Math.floor(diff / 60)}分前`;
	if (diff < 86400) return `${Math.floor(diff / 3600)}時間前`;
	if (diff < 86400 * 30) return `${Math.floor(diff / 86400)}日前`;
	return `${Math.floor(diff / (86400 * 30))}か月前`;
}
