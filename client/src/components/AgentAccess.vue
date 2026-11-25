<template>
  <v-dialog :value="true" :persistent="busy" max-width="760px" @input="onDialogInput">
    <v-card>
      <v-card-title class="headline text-break">Agent · {{ roomName }}</v-card-title>
      <v-card-text>
        <v-alert type="info" outlined dense>
          Temporary keys authorize Agent access to this room only. Restarting Manager invalidates all temporary keys.
        </v-alert>

        <v-row dense>
          <v-col cols="12" sm="4">
            <v-select v-model="ttlSeconds" :items="ttlPresets" label="Lifetime" outlined dense
              :disabled="creating" hide-details />
          </v-col>
          <v-col cols="12" sm="8">
            <v-text-field v-model="label" label="Label (optional)" outlined dense
              :disabled="creating" hide-details />
          </v-col>
        </v-row>
        <v-btn class="my-4" color="primary" :loading="creating" :disabled="busy"
          @click="CreateKey">
          Create temporary key and copy instructions
        </v-btn>
        <v-alert v-if="createError" type="error" outlined dense role="alert">{{ createError }}</v-alert>

        <section v-if="issued" class="mb-6" aria-label="New Agent instructions">
          <div class="text-break">Room: {{ issued.room_name }}</div>
          <div class="mb-2">Expires (local time): {{ formatTime(issued.expires_at) }}</div>
          <label for="agent-prompt" class="d-block mb-1">Instructions (shown once; copy and give to your Agent)</label>
          <textarea id="agent-prompt" ref="prompt" class="agent-prompt" :value="issued.prompt"
            readonly rows="8" spellcheck="false" />
          <v-btn small outlined class="my-2" @click="CopyPrompt">Copy instructions</v-btn>
          <v-alert v-if="copyStatus === 'copied'" type="success" outlined dense role="status">
            Instructions copied. Paste them into your Agent.
          </v-alert>
          <v-alert v-else-if="copyStatus === 'manual'" type="warning" outlined dense role="status">
            Automatic copy failed. The instructions are selected. Press Ctrl+C (Mac: ⌘C), or copy using the system menu.
          </v-alert>
          <div class="text-caption">Closing this dialog does not revoke the key. Later you can view its expiration or revoke it, but cannot retrieve these instructions again.</div>
        </section>

        <div class="d-flex align-center">
          <h3 class="text-subtitle-1">Active keys</h3>
          <v-spacer />
          <v-btn text small :loading="loading" :disabled="busy" @click="LoadKeys">Refresh list</v-btn>
        </div>
        <div class="text-caption mb-2">Connection counts reflect the latest refresh. Revoking a key disconnects all of its connections.</div>
        <v-alert v-if="listError" type="error" outlined dense role="alert">{{ listError }}</v-alert>
        <v-alert v-if="revokeError" type="error" outlined dense role="alert">{{ revokeError }}</v-alert>
        <v-progress-linear v-if="loading" indeterminate aria-label="Loading active keys" />
        <v-simple-table v-if="keys.length" dense>
          <thead>
            <tr><th>Label</th><th>Expires (local time)</th><th>Connections</th><th>Actions</th></tr>
          </thead>
          <tbody>
            <tr v-for="key in keys" :key="key.key_id">
              <td class="text-break">{{ key.label || 'Unnamed' }}</td>
              <td>{{ formatTime(key.expires_at) }}</td>
              <td>{{ key.active_connections }}</td>
              <td>
                <v-btn small text color="error" :loading="revoking === key.key_id" :disabled="busy"
                  :aria-label="'Revoke ' + (key.label || 'Unnamed key')" @click="RevokeKey(key.key_id)">Revoke</v-btn>
              </td>
            </tr>
          </tbody>
        </v-simple-table>
        <div v-else-if="!loading && !listError" class="py-3">This room has no active keys.</div>
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn text :disabled="busy" @click="$emit('close')">Close</v-btn>
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
    { text: '15 minutes', value: 900 },
    { text: '1 hour', value: 3600 },
    { text: '8 hours', value: 28800 },
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
      if (request === this.listRequest) this.listError = 'Could not load keys: ' + agentErrorMessage(error)
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
      this.createError = 'Could not create key: ' + agentErrorMessage(error)
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
      this.revokeError = 'Could not revoke key: ' + agentErrorMessage(error)
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
