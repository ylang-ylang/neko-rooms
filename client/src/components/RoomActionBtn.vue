<template>
  <v-tooltip bottom v-if="tmpl" open-delay="300">
    <template v-slot:activator="{ on, attrs }">
      <v-btn v-bind="attrs" v-on="on" @click="Action" :color="tmpl.color" :disabled="disabled" :loading="loading" :aria-label="tmpl.tooltip"
        icon><v-icon>{{ tmpl.icon }}</v-icon></v-btn>
    </template>
    <span>{{ tmpl.tooltip }}</span>
  </v-tooltip>
</template>

<script lang="ts">
import { Vue, Component, Prop } from 'vue-property-decorator'
import { AxiosError } from 'axios'

@Component
export default class RoomActionBtn extends Vue {
  @Prop(String) readonly action!: string
  @Prop(String) readonly roomId!: string
  @Prop(Boolean) readonly disabled!: boolean

  public loading = false

  // eslint-disable-next-line
  get tmpl(): any | undefined {
    switch (this.action) {
      case 'start':
        return {
          dispatch: 'ROOMS_START',
          msg: 'Room started!',
          tooltip: 'Start',
          color: 'green',
          icon: 'mdi-play-circle-outline',
        }
      case 'stop':
        return {
          dispatch: 'ROOMS_STOP',
          msg: 'Room stopped!',
          tooltip: 'Stop',
          color: 'warning',
          icon: 'mdi-stop-circle-outline',
        }
      case 'pause':
        return {
          dispatch: 'ROOMS_PAUSE',
          msg: 'Room paused!',
          tooltip: 'Pause',
          color: 'orange',
          icon: 'mdi-pause-circle-outline',
        }
      case 'restart':
        return {
          dispatch: 'ROOMS_RESTART',
          msg: 'Room restarted!',
          tooltip: 'Restart',
          color: 'blue',
          icon: 'mdi-refresh',
        }
      case 'recreate':
        return {
          dispatch: 'ROOMS_RECREATE',
          msg: 'Room recreated!',
          tooltip: 'Recreate',
          color: 'blue',
          icon: 'mdi-cloud-refresh',
        }
      case 'remove':
        return {
          dispatch: 'ROOMS_REMOVE',
          msg: 'Room removed!',
          tooltip: 'Remove',
          color: 'red',
          icon: 'mdi-trash-can-outline',
        }
    }

    return undefined
  }

  async Action() {
    if (!this.tmpl || this.loading) return
    this.loading = true

    try {
      const room = this.$store.state.rooms.find((entry: { id: string }) => entry.id === this.roomId)
      const profileId = room?.profile_id
      let deleteProfile = false
      if (this.action === 'remove') {
        const result = await this.$swal({
          title: 'Remove deployment?',
          customClass: { popup: 'room-removal-confirmation' },
          text: profileId
            ? `Remove "${room.name}"? Profile "${room.profile_name || profileId}" is kept by default. Independent profile copies made by cloning are not affected.`
            : 'Remove this deployment? Container-only data will be lost.',
          input: profileId ? 'checkbox' : undefined,
          inputValue: 0,
          inputPlaceholder: 'Also permanently delete this profile, including cookies, history, extensions, site data and Downloads. This cannot be undone.',
          icon: 'warning',
          showCancelButton: true,
          confirmButtonText: 'Remove deployment',
          cancelButtonText: 'Cancel',
          confirmButtonColor: '#d32f2f',
        })
        if (!result.isConfirmed) return
        deleteProfile = !!profileId && result.value === 1
      } else if (this.action === 'recreate') {
        const result = await this.$swal({
          title: 'Recreate room',
          text: 'Recreate this deployment using the same durable profile? Container-only data will be lost.',
          icon: 'warning',
          showCancelButton: true,
          confirmButtonText: 'Yes',
          cancelButtonText: 'No',
        })
        if (!result.isConfirmed) return
      }

      await this.$store.dispatch(this.tmpl.dispatch, this.roomId)
      if (this.action === 'remove' && profileId) {
        if (deleteProfile) {
          try {
            // The server still rejects deletion if another deployment has bound this profile.
            await this.$store.dispatch('PROFILES_DELETE', profileId)
          } catch (error) {
            const response = (error as AxiosError).response
            await this.$swal({
              title: 'Deployment removed; profile deletion not confirmed',
              text: `${response ? String(response.data) : 'The network request failed.'} Refresh Profiles before trying to delete its data. Do not remove the deployment again.`,
              icon: 'warning',
            })
            return
          }
        }
        try {
          await this.$store.dispatch('PROFILES_LOAD')
        } catch {
          await this.$swal({
            title: deleteProfile ? 'Deployment and profile deleted' : 'Deployment removed; profile kept',
            text: 'The list could not refresh. Refresh it manually; do not repeat the deletion.',
            icon: 'warning',
          })
          return
        }
      }
      await this.$swal({
        title: this.action === 'remove' && profileId
          ? (deleteProfile ? 'Deployment and profile deleted' : 'Deployment removed; profile kept')
          : this.tmpl.msg,
        icon: 'success',
      })
    } catch (e) {
      const response = (e as AxiosError).response
      await this.$swal({
        title: response ? 'Server error' : 'Network error',
        text: response ? String(response.data) : String(e),
        icon: 'error',
      })
    } finally {
      this.loading = false
    }
  }
}
</script>

<style>
.room-removal-confirmation .swal2-checkbox { align-items: flex-start; justify-content: flex-start; text-align: left; background: transparent; }
.room-removal-confirmation .swal2-checkbox .swal2-label { color: #dcddde; opacity: 1; font-size: 14px; line-height: 1.4; }
.room-removal-confirmation .swal2-checkbox input { flex-shrink: 0; margin-top: 3px; }
</style>
