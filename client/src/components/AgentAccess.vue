<template>
  <v-dialog :value="true" :persistent="busy" max-width="760px" @input="onDialogInput">
    <v-card>
      <v-card-title class="headline text-break">Agent · {{ roomName }}</v-card-title>
      <v-card-text>
        <v-alert type="info" outlined dense>
          临时 Key 只用于此房间的 Agent 访问。Manager 重启会使所有临时 Key 失效。
        </v-alert>

        <v-row dense>
          <v-col cols="12" sm="4">
            <v-select v-model="ttlSeconds" :items="ttlPresets" label="有效期" outlined dense
              :disabled="creating" hide-details />
          </v-col>
          <v-col cols="12" sm="8">
            <v-text-field v-model="label" label="标签（可选）" outlined dense
              :disabled="creating" hide-details />
          </v-col>
        </v-row>
        <v-btn class="my-4" color="primary" :loading="creating" :disabled="busy"
          @click="CreateKey">
          创建临时 Key 并复制提示词
        </v-btn>
        <v-alert v-if="createError" type="error" outlined dense role="alert">{{ createError }}</v-alert>

        <section v-if="issued" class="mb-6" aria-label="本次创建的 Agent 提示词">
          <div class="text-break">房间：{{ issued.room_name }}</div>
          <div class="mb-2">到期（本地时间）：{{ formatTime(issued.expires_at) }}</div>
          <label for="agent-prompt" class="d-block mb-1">提示词（仅本次显示，请复制后交给 Agent）</label>
          <textarea id="agent-prompt" ref="prompt" class="agent-prompt" :value="issued.prompt"
            readonly rows="8" spellcheck="false" />
          <v-btn small outlined class="my-2" @click="CopyPrompt">复制提示词</v-btn>
          <v-alert v-if="copyStatus === 'copied'" type="success" outlined dense role="status">
            已复制提示词，可直接粘贴给 Agent。
          </v-alert>
          <v-alert v-else-if="copyStatus === 'manual'" type="warning" outlined dense role="status">
            自动复制未成功。提示词已选中，请按 Ctrl+C（Mac：⌘C），或使用系统菜单复制。
          </v-alert>
          <div class="text-caption">关闭对话框不会撤销 Key；之后只能查看有效期或撤销，不能再次取回提示词。</div>
        </section>

        <div class="d-flex align-center">
          <h3 class="text-subtitle-1">有效 Key</h3>
          <v-spacer />
          <v-btn text small :loading="loading" :disabled="busy" @click="LoadKeys">刷新列表</v-btn>
        </div>
        <div class="text-caption mb-2">连接数以最近一次刷新为准。撤销会断开该 Key 的所有连接。</div>
        <v-alert v-if="listError" type="error" outlined dense role="alert">{{ listError }}</v-alert>
        <v-alert v-if="revokeError" type="error" outlined dense role="alert">{{ revokeError }}</v-alert>
        <v-progress-linear v-if="loading" indeterminate aria-label="加载有效 Key" />
        <v-simple-table v-if="keys.length" dense>
          <thead>
            <tr><th>标签</th><th>到期（本地时间）</th><th>连接数</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="key in keys" :key="key.key_id">
              <td class="text-break">{{ key.label || '未命名' }}</td>
              <td>{{ formatTime(key.expires_at) }}</td>
              <td>{{ key.active_connections }}</td>
              <td>
                <v-btn small text color="error" :loading="revoking === key.key_id" :disabled="busy"
                  :aria-label="'撤销 ' + (key.label || '未命名 Key')" @click="RevokeKey(key.key_id)">撤销</v-btn>
              </td>
            </tr>
          </tbody>
        </v-simple-table>
        <div v-else-if="!loading && !listError" class="py-3">此房间暂无有效 Key。</div>
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn text :disabled="busy" @click="$emit('close')">关闭</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>

<script lang="ts">
import { Vue, Component, Prop } from 'vue-property-decorator'
import {
  AgentKey,
  IssuedAgentKey,
  agentErrorMessage,
  createAgentKey,
  listAgentKeys,
  revokeAgentKey,
} from '@/api/agent'
import { copyPrompt } from '@/utils/clipboard'

@Component
export default class AgentAccess extends Vue {
  @Prop({ type: String, required: true }) readonly roomName!: string

  public ttlSeconds = 3600
  public ttlPresets = [
    { text: '15 分钟', value: 900 },
    { text: '1 小时', value: 3600 },
    { text: '8 小时', value: 28800 },
  ]
  public label = ''
  public keys: AgentKey[] = []
  public issued: IssuedAgentKey | null = null
  public creating = false
  public loading = false
  public revoking = ''
  public createError = ''
  public listError = ''
  public revokeError = ''
  public copyStatus: 'idle' | 'copied' | 'manual' = 'idle'
  private listRequest = 0

  get busy(): boolean {
    return this.creating || this.revoking !== ''
  }

  onDialogInput(open: boolean): void {
    if (!open && !this.busy) this.$emit('close')
  }

  formatTime(value: string): string {
    const date = new Date(value)
    return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
  }

  async LoadKeys(): Promise<void> {
    const request = ++this.listRequest
    this.loading = true
    this.listError = ''
    try {
      const keys = await listAgentKeys(this.roomName)
      if (request === this.listRequest) this.keys = keys
    } catch (error) {
      if (request === this.listRequest) this.listError = '加载 Key 失败：' + agentErrorMessage(error)
    } finally {
      if (request === this.listRequest) this.loading = false
    }
  }

  async CreateKey(): Promise<void> {
    if (this.busy) return
    this.creating = true
    this.createError = ''
    try {
      this.issued = await createAgentKey(this.roomName, this.ttlSeconds, this.label)
      this.copyStatus = 'idle'
      await this.$nextTick()
      // Copy feedback is inline; neither clipboard permission nor a modal can block key actions.
      void this.CopyPrompt()
      void this.LoadKeys()
    } catch (error) {
      this.createError = '创建 Key 失败：' + agentErrorMessage(error)
    } finally {
      this.creating = false
    }
  }

  async CopyPrompt(): Promise<void> {
    const issued = this.issued
    const textarea = this.$refs.prompt as HTMLTextAreaElement | undefined
    if (!issued || !textarea) return
    this.copyStatus = 'idle'
    const copied = await copyPrompt(textarea)
    if (this.issued === issued) this.copyStatus = copied ? 'copied' : 'manual'
  }

  async RevokeKey(keyId: string): Promise<void> {
    if (this.busy) return
    this.revoking = keyId
    this.revokeError = ''
    try {
      await revokeAgentKey(this.roomName, keyId)
      // DELETE completes only after the backend has closed this key's sessions.
      this.keys = this.keys.filter(key => key.key_id !== keyId)
      if (this.issued && this.issued.key_id === keyId) {
        this.issued = null
        this.copyStatus = 'idle'
      }
      void this.LoadKeys()
    } catch (error) {
      this.revokeError = '撤销 Key 失败：' + agentErrorMessage(error)
    } finally {
      this.revoking = ''
    }
  }

  mounted(): void {
    void this.LoadKeys()
  }
}
</script>

<style scoped>
.agent-prompt {
  display: block;
  width: 100%;
  padding: 12px;
  border: 1px solid currentColor;
  border-radius: 4px;
  color: inherit;
  font-family: monospace;
  resize: vertical;
}
</style>
