<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkFolder>
	<template #label>再生中の音楽</template>
	<template #suffix>{{ current || '未設定' }}</template>

	<div class="_gaps_m">
		<!--
			MkSelect はプラグインに公開されていないので、ボタンで選ばせる。
			選択肢が 2 つしかないので、これで十分に分かる。
		-->
		<div>
			<div :class="$style.caption">サービス</div>
			<div class="_buttons">
				<MkButton
					v-for="s in services"
					:key="s"
					:primary="service === s"
					@click="service = s"
				>{{ serviceLabel(s) }}</MkButton>
			</div>
			<div v-if="!lastFmAvailable" :class="$style.note">
				このサーバーでは Last.fm の API キーが設定されていないため、ListenBrainz のみ使えます。
			</div>
		</div>

		<MkInput v-model="draft" type="text" :placeholder="'username'">
			<template #label>ユーザー名</template>
			<template #caption>
				{{ serviceLabel(service) }} のユーザー名を入れると、聴いている曲がプロフィールに表示されます。
				空にすると連携を解除します。
			</template>
		</MkInput>

		<div class="_buttons">
			<MkButton primary :disabled="saving" @click="save">
				<template v-if="saving"><MkLoading :em="true"/></template>
				<template v-else>保存</template>
			</MkButton>
		</div>

		<!--
			結果はここに出す。バックエンドが返した理由 (ユーザー名が見つからない等)
			をそのまま見せる — 利用者が直せるものなので。
		-->
		<div v-if="message" :class="failed ? $style.error : $style.ok">{{ message }}</div>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { MkInput, MkButton, MkFolder, MkLoading } from '@/plugin-api.js';
import { api, serviceLabel, SERVICES } from './api.js';
import type { MeResponse } from './api.js';

const service = ref<string>('listenbrainz');
const draft = ref('');
const saved = ref<MeResponse | null>(null);
const lastFmAvailable = ref(true);
const saving = ref(false);
const message = ref('');
const failed = ref(false);

// API キーが無いインスタンスでは Last.fm を選ばせない。選ばせてから
// 「使えません」と言うより親切。
const services = computed(() => SERVICES.filter((s) => s !== 'lastfm' || lastFmAvailable.value));

const current = computed(() => {
	const me = saved.value;
	if (me?.username == null) return '';
	return `${serviceLabel(me.service ?? '')} · ${me.username}`;
});

onMounted(async () => {
	try {
		const me = await api<MeResponse>('me', {});
		saved.value = me;
		lastFmAvailable.value = me.lastFmAvailable;
		if (me.service != null) service.value = me.service;
		draft.value = me.username ?? '';
	} catch (err) {
		// 現在値が読めなくても入力欄は使えるままにする。
		console.error('[plugin:nowplaying] 現在の設定を取得できませんでした', err);
	}
});

async function save(): Promise<void> {
	saving.value = true;
	message.value = '';
	try {
		const res = await api<MeResponse>('me/set', {
			service: service.value,
			username: draft.value.trim(),
		});
		saved.value = { ...res, lastFmAvailable: lastFmAvailable.value };
		failed.value = false;
		message.value = res.username == null ? '連携を解除しました' : '保存しました';
	} catch (err) {
		failed.value = true;
		message.value = (err as { message?: string } | null)?.message ?? '保存に失敗しました';
	} finally {
		saving.value = false;
	}
}
</script>

<style lang="scss" module>
.caption {
	font-size: 0.85em;
	opacity: 0.75;
	margin-bottom: 6px;
}

.note {
	margin-top: 6px;
	font-size: 0.85em;
	opacity: 0.7;
}

.ok {
	color: var(--MI_THEME-success);
	font-size: 0.9em;
}

.error {
	color: var(--MI_THEME-error);
	font-size: 0.9em;
}
</style>
