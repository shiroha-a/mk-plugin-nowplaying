/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { definePlugin } from '@/plugin-api.js';
import { initApi } from './api.js';
import ProfileCard from './ProfileCard.vue';
import SettingsSection from './SettingsSection.vue';

export default definePlugin({
	name: 'nowplaying',

	setup(host) {
		initApi(host.api);

		// Vue コンポーネント形式で登録する。ホストのアプリ内で描画されるので
		// MkInput などが本体と同じ見た目・挙動で動く。
		host.slot('profile:info', { component: ProfileCard });

		// 未ログインでは設定画面自体が出ないが、念のため。
		if (host.me != null) {
			host.slot('settings:profile', { component: SettingsSection });
		}
	},
});
