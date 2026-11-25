<template>
  <div>
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
        <div class="room-name">{{ item.name }}</div>
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
        <div class="room-actions">
          <RoomActionBtn action="start" :roomId="item.id" :disabled="item.running" />
          <RoomActionBtn action="stop" :roomId="item.id" :disabled="!item.running && !item.paused" />
          <RoomActionBtn action="pause" :roomId="item.id" :disabled="!item.running" />
          <RoomActionBtn action="restart" :roomId="item.id" :disabled="!item.running" />
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
  min-width: 10rem;
  overflow-wrap: anywhere;
}
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
</style>
