<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="headline" :class="$style.root">
	<!--
		既定は 1 行だけ。何を聴いているかが分かれば足りるので、履歴は
		開いたときだけ出す。
	-->
	<button type="button" class="_button" :class="$style.label" :aria-expanded="open" @click="open = !open">
		<img v-if="headline.art" :class="$style.labelArt" :src="headline.art" alt=""/>
		<i v-else :class="[$style.labelIcon, 'ti ti-music']"></i>

		<span v-if="playing" :class="$style.playing">再生中</span>
		<span v-else :class="$style.recentLabel">最近</span>

		<span :class="$style.title">{{ headline.title }}</span>
		<span :class="$style.artist">{{ headline.artist }}</span>
		<span v-if="!playing && headline.listenedAt" :class="$style.when">{{ relativeTime(headline.listenedAt) }}</span>

		<i :class="[$style.chevron, open ? 'ti ti-chevron-up' : 'ti ti-chevron-down']"></i>
	</button>

	<div v-if="open" :class="$style.panel">
		<div v-if="recent.length > 0" :class="$style.list">
			<a
				v-for="(t, i) in recent"
				:key="i"
				:class="$style.item"
				:href="t.url || undefined"
				rel="nofollow noopener"
				target="_blank"
			>
				<img v-if="t.art" :class="$style.itemArt" :src="t.art" alt=""/>
				<span v-else :class="$style.itemArtBlank"></span>
				<span :class="$style.itemBody">
					<span :class="$style.itemTitle">{{ t.title }}</span>
					<span :class="$style.itemArtist">{{ t.artist }}</span>
				</span>
				<span :class="$style.itemWhen">{{ relativeTime(t.listenedAt) }}</span>
			</a>
		</div>
		<div v-else :class="$style.empty">履歴がありません</div>

		<div :class="$style.footer">{{ serviceLabel(data!.service) }} · {{ data!.username }}</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { type SlotContext } from '@/plugin-api.js';
import { api, relativeTime, serviceLabel } from './api.js';
import type { ProfileResponse, LinkedProfile, Track } from './api.js';

const props = defineProps<{ ctx: SlotContext }>();

const data = ref<LinkedProfile | null>(null);
const open = ref(false);

const playing = computed<Track | null>(() => data.value?.playing ?? null);
const recent = computed<Track[]>(() => data.value?.recent ?? []);

/*
 * ラベルに出す 1 曲。
 *
 * 再生中があればそれ、無ければ直近に聴いたもの。**どちらも無ければ行ごと
 * 出さない** — 登録しているのに空の行が残ると、壊れているように見える。
 */
const headline = computed<Track | null>(() => playing.value ?? recent.value[0] ?? null);

onMounted(async () => {
	const user = props.ctx.user;
	if (user == null) return;
	// リモート利用者も引く。相手が同じプラグインを入れた mk-go なら、
	// バックエンドが取り寄せて返す (初回は間に合わないので出ない)。

	try {
		const res = await api<ProfileResponse>('profile', { userId: user.id });
		if (res.linked) data.value = res;
	} catch (err) {
		// 表示できないだけで済ませる。プロフィール全体を壊さない。
		console.error('[plugin:nowplaying] プロフィールの取得に失敗しました', err);
	}
});
</script>

<style lang="scss" module>
.root {
	margin: 8px 0;
}

.label {
	display: flex;
	align-items: center;
	gap: 8px;
	width: 100%;
	padding: 6px 10px;
	border-radius: var(--MI-radius-sm, 8px);
	background: var(--MI_THEME-buttonBg);
	font-size: 0.9em;
	text-align: left;

	&:hover {
		background: var(--MI_THEME-buttonHoverBg);
	}
}

.labelArt {
	width: 24px;
	height: 24px;
	border-radius: 4px;
	object-fit: cover;
	background: var(--MI_THEME-bg);
	flex-shrink: 0;
}

.labelIcon {
	width: 24px;
	text-align: center;
	opacity: 0.6;
	flex-shrink: 0;
}

/* 再生中だけ色を付ける。今なのか過去なのかが一目で分かるように。 */
.playing {
	color: var(--MI_THEME-accent);
	font-weight: 700;
	font-size: 0.85em;
	flex-shrink: 0;
}

.recentLabel {
	opacity: 0.55;
	font-size: 0.85em;
	flex-shrink: 0;
}

.title {
	font-weight: 600;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.artist {
	opacity: 0.7;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
	flex-shrink: 2;
}

.when {
	opacity: 0.5;
	font-size: 0.85em;
	flex-shrink: 0;
}

.chevron {
	margin-left: auto;
	opacity: 0.6;
	flex-shrink: 0;
	padding-left: 4px;
}

.panel {
	margin-top: 6px;
	padding: 8px 10px;
	border-radius: var(--MI-radius, 12px);
	background: var(--MI_THEME-panel);
	border: 1px solid var(--MI_THEME-divider);
}

.list {
	display: flex;
	flex-direction: column;
}

.item {
	display: flex;
	align-items: center;
	gap: 8px;
	padding: 4px 2px;
	color: inherit;
	text-decoration: none;
	border-radius: 6px;

	&:hover {
		background: var(--MI_THEME-buttonBg);
	}
}

.itemArt,
.itemArtBlank {
	width: 32px;
	height: 32px;
	border-radius: 4px;
	object-fit: cover;
	background: var(--MI_THEME-bg);
	flex-shrink: 0;
}

.itemBody {
	display: flex;
	flex-direction: column;
	min-width: 0;
	flex: 1;
	font-size: 0.9em;
}

.itemTitle {
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.itemArtist {
	opacity: 0.65;
	font-size: 0.9em;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.itemWhen {
	opacity: 0.5;
	font-size: 0.8em;
	flex-shrink: 0;
}

.empty {
	opacity: 0.6;
	font-size: 0.9em;
	padding: 4px 2px;
}

.footer {
	margin-top: 8px;
	font-size: 0.75em;
	opacity: 0.55;
}
</style>
