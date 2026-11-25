<template>
  <div>
    <v-alert v-if="cloneSuccess" type="success" outlined dense role="status" class="text-break">{{ cloneSuccess }}</v-alert>
    <v-alert v-if="cloneRefreshError" type="warning" outlined dense role="alert">{{ cloneRefreshError }}</v-alert>
    <v-data-table :headers="headers" :items="rooms" class="room-list elevation-1" :loading="loading"
      loading-text="Loading... Please wait" hide-default-footer>
      <template v-slot:[`item.url`]="{ item }">
        <v-tooltip bottom open-delay="300">
          <template v-slot:activator="{ on, attrs }">
            <v-btn v-bind="attrs" v-on="on" @click="roomId = item.id; dialog = true" color="blue" small class="mr-2">
              <v-icon small>mdi-information-outline</v-icon></v-btn>
          </template>
          <span>View more information</span>
        </v-tooltip>
        <v-tooltip bottom open-delay="300">
          <template v-slot:activator="{ on, attrs }">
            <v-btn v-bind="attrs" v-on="on" :disabled="!item.running" :href="item.url" target="_blank" small> <v-icon
                small>mdi-open-in-new</v-icon></v-btn>
          </template>
          <span>Link to deployment</span>
        </v-tooltip>
      </template>
      <template v-slot:[`item.name`]="{ item }">
        <div class="room-name">
          <div class="room-title">{{ item.name }}</div>
          <v-tooltip v-if="item.profile_id" bottom open-delay="300">
            <template v-slot:activator="{ on, attrs }">
              <v-chip v-bind="attrs" v-on="on" small label outlined color="teal lighten-2" class="room-profile" :aria-label="'Browser profile: ' + item.profile_name">
                <v-icon left small>mdi-account-circle-outline</v-icon>
                <span class="room-profile-name">{{ item.profile_name }}</span>
              </v-chip>
            </template>
            <span class="room-profile-tooltip">{{ item.profile_name }} ({{ item.profile_id }})</span>
          </v-tooltip>
        </div>
      </template>
      <template v-slot:[`item.neko_image`]="{ item }">
        <div class="room-image d-flex align-center">
          <RoomActionBtn action="recreate" :roomId="item.id" />
          <span class="ml-3" :title="item.neko_image">{{ item.neko_image }}</span>
          <v-tooltip bottom open-delay="300" v-if="item.is_outdated">
            <template v-slot:activator="{ on, attrs }">
              <v-icon v-bind="attrs" v-on="on" class="ml-2" color="warning">mdi-update</v-icon>
            </template>
            <div class="text-center">This image is outdated. <br> Recreate room to update it.</div>
          </v-tooltip>
        </div>
      </template>
      <template v-slot:[`item.max_connections`]="{ item }">
        <span v-if="item.max_connections > 0">{{ item.max_connections }}</span>
        <i v-else>uses mux</i>
      </template>
      <template v-slot:[`item.status`]="{ item }">
        <v-chip :color="item.running ? (item.status.includes('unhealthy') ? 'warning' : 'green') : 'red'" dark small> {{
          item.status }} </v-chip>
      </template>
      <template v-slot:[`item.created`]="{ item }">
        {{ item.created | timeago }}
      </template>
      <template v-slot:[`item.actions`]="{ item }">
        <div class="room-actions" :class="{ 'room-actions--compact': !$store.state.roomsConfig.uses_mux }">
          <RoomActionBtn action="start" :roomId="item.id" :disabled="item.running" />
          <RoomActionBtn action="stop" :roomId="item.id" :disabled="!item.running && !item.paused" />
          <RoomActionBtn action="pause" :roomId="item.id" :disabled="!item.running" />
          <RoomActionBtn action="restart" :roomId="item.id" :disabled="!item.running" />
          <v-tooltip bottom open-delay="300">
            <template v-slot:activator="{ on, attrs }">
              <v-btn v-bind="attrs" v-on="on" icon color="primary" :disabled="!item.id || !item.name"
                :aria-label="'Clone deployment ' + item.name"
                @click="OpenClone(item)"><v-icon>mdi-content-copy</v-icon></v-btn>
            </template>
            <span>Clone deployment</span>
          </v-tooltip>
        </div>
      </template>
      <template v-slot:[`item.agent`]="{ item }">
        <v-btn small outlined color="primary" :disabled="!item.name" @click="agentRoomName = item.name">
          <v-icon small left>mdi-robot-outline</v-icon>Agent
        </v-btn>
      </template>
      <template v-slot:[`item.destroy`]="{ item }">
        <RoomActionBtn action="remove" :roomId="item.id" />
      </template>
    </v-data-table>

    <AgentAccess v-if="agentRoomName" :key="agentRoomName" :roomName="agentRoomName" @close="agentRoomName = ''" />

    <v-dialog :value="!!cloneSource" :persistent="cloneLoading" max-width="480px"
      aria-labelledby="room-clone-title" @input="onCloneDialogInput">
      <v-card v-if="cloneSource" :aria-busy="cloneLoading">
        <v-card-title id="room-clone-title" class="headline">Clone deployment</v-card-title>
        <v-form ref="cloneForm" @submit.prevent="Clone">
          <v-card-text>
            <p class="text-break">Source deployment: {{ cloneSource.name }}</p>
            <p>Fork an independent durable profile, copy deployment settings and start the new deployment.</p>
            <v-text-field v-model="cloneName" label="New deployment name" :rules="cloneNameRules"
              :disabled="cloneLoading" autofocus autocomplete="off" @focus="selectCloneName" @input="cloneError = ''" />
            <v-alert v-if="cloneError" type="error" outlined dense role="alert">{{ cloneError }}</v-alert>
            <v-progress-linear v-if="cloneLoading" indeterminate aria-label="Cloning and starting deployment" />
          </v-card-text>
          <v-card-actions>
            <v-spacer />
            <v-btn text :disabled="cloneLoading" @click="CloseClone">Cancel</v-btn>
            <v-btn type="submit" color="primary" :loading="cloneLoading" :disabled="cloneLoading">Clone and start</v-btn>
          </v-card-actions>
        </v-form>
      </v-card>
    </v-dialog>

    <v-dialog v-model="dialog" max-width="920px">
      <v-card>
        <v-card-title class="headline">
          Room information
        </v-card-title>
        <v-card-text>
          <RoomInfo v-if="dialog" :roomId="roomId" />
        </v-card-text>
        <v-card-actions>
          <v-spacer></v-spacer>
          <v-btn color="grey" text @click="dialog = false">
            Close
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>
  </div>
</template>

<script lang="ts">
import { Vue, Component, Prop } from 'vue-property-decorator'
import axios from 'axios'
import { RoomEntry } from '@/api'
import { roomNameRules } from '@/utils/room-name'
import RoomInfo from '@/components/RoomInfo.vue'
import RoomActionBtn from '@/components/RoomActionBtn.vue'
import AgentAccess from '@/components/AgentAccess.vue'

@Component({
  components: {
    RoomInfo,
    RoomActionBtn,
    AgentAccess,
  }
})
export default class RoomsList extends Vue {
  @Prop(Boolean) readonly loading!: boolean

  public dialog = false
  public roomId = ''
  public agentRoomName = ''
  public roomLoading = [] as Array<string>
  public cloneSource: RoomEntry | null = null
  public cloneName = ''
  public cloneLoading = false
  public cloneError = ''
  public cloneSuccess = ''
  public cloneRefreshError = ''
  public cloneNameRules = [
    (value: string) => !!value || 'Enter a new deployment name.',
    ...roomNameRules,
  ]

  OpenClone(room: RoomEntry): void {
    if (this.cloneLoading || !room.id || !room.name) return
    this.cloneSource = room
    this.cloneName = room.name + '-copy'
    this.cloneError = ''
    this.cloneSuccess = ''
    this.cloneRefreshError = ''
  }

  selectCloneName(event: FocusEvent): void {
    (event.target as HTMLInputElement).select()
  }

  onCloneDialogInput(open: boolean): void {
    if (!open) this.CloseClone()
  }

  CloseClone(): void {
    // A POST already in flight is not cancellable; closing must not imply rollback.
    if (this.cloneLoading) return
    this.cloneSource = null
    this.cloneName = ''
    this.cloneError = ''
  }

  async Clone(): Promise<void> {
    if (this.cloneLoading || !this.cloneSource) return
    const form = this.$refs.cloneForm as Vue & { validate: () => boolean }
    if (!form.validate()) return
    const roomId = this.cloneSource.id
    const name = this.cloneName
    this.cloneLoading = true
    this.cloneError = ''
    try {
      await this.$store.dispatch('ROOMS_CLONE', { roomId, name })
      this.cloneSuccess = `Deployment "${name}" was created and started. Check its readiness in the list.`
      this.cloneSource = null
      this.cloneName = ''
      try {
        await this.$store.dispatch('ROOMS_LOAD')
      } catch {
        // Creation has committed. A refresh failure must never invite another clone POST.
        this.cloneRefreshError = 'Deployment created, but list refresh failed. Refresh the list manually; do not clone again.'
      }
    } catch (error) {
      if (axios.isAxiosError(error) && error.response) {
        const { status, data } = error.response
        const detail = typeof data === 'string' ? data.trim() :
          (typeof data?.message === 'string' ? data.message : typeof data?.error === 'string' ? data.error : '')
        this.cloneError = status === 409 ? 'A deployment with this name already exists. Choose another name. (HTTP 409)' :
          `Clone failed (HTTP ${status})${detail ? ': ' + detail : '. Please retry.'}`
      } else {
        this.cloneError = 'Network request failed; clone outcome is unconfirmed. Refresh the deployment list before retrying.'
      }
    } finally {
      this.cloneLoading = false
    }
  }

  get headers() {
    return [
      {
        text: 'Deployment',
        value: 'url',
        sortable: false,
      },
      { text: 'Name', value: 'name' },
      // when using mux we don't have max connections
      ...(!this.$store.state.roomsConfig.uses_mux ? [
        { text: 'Max connections', value: 'max_connections' }
      ] : []),
      { text: 'Neko image', value: 'neko_image' },
      { text: 'Status', value: 'status' },
      { text: 'Created', value: 'created' },
      {
        text: 'Actions',
        value: 'actions',
        sortable: false,
      },
      { text: 'Agent', value: 'agent', sortable: false },
      {
        text: 'Destroy',
        value: 'destroy',
        align: 'end',
        sortable: false,
      },
    ]
  }

  get rooms() {
    return this.$store.state.rooms
  }

  async Reload(roomId: string) {
    Vue.set(this, 'roomLoading', [roomId, ...this.roomLoading])

    try {
      await this.$store.dispatch('ROOMS_GET', roomId)
    } finally {
      const roomLoading = this.roomLoading.filter((id: string) => id != roomId)
      Vue.set(this, 'roomLoading', roomLoading)
    }
  }
}
</script>

<style scoped>
.room-name {
  width: 12rem;
  min-width: 10rem;
  max-width: 14rem;
  overflow-wrap: anywhere;
}
.room-title { font-weight: 600; line-height: 1.4; }
.room-profile { max-width: 100%; margin-top: 5px; }
.room-profile-tooltip { display: block; max-width: min(400px, calc(100vw - 48px)); white-space: normal; overflow-wrap: anywhere; }
.room-profile ::v-deep .v-chip__content { min-width: 0; max-width: 100%; }
.room-profile-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.room-image {
  min-width: 12rem;
  max-width: 24rem;
}
.room-image span {
  min-width: 0;
  overflow-wrap: anywhere;
}
.room-actions {
  white-space: nowrap;
}
@media (min-width: 600px) {
  /* The optional connections column needs a narrower action cell, not smaller targets. */
  .room-actions--compact {
    display: grid;
    grid-template-columns: repeat(3, 36px);
    row-gap: 4px;
    width: max-content;
  }
}
</style>
