<template>
  <v-card>
    <v-card-title>
      <span class="headline"> Create new Room </span>
    </v-card-title>
    <v-card-text>
      <v-form
        ref="form"
        v-model="valid"
        lazy-validation
      >
        <v-row align="center">
          <v-col class="pb-0">
            <v-text-field
              label="Name"
              v-model="data.name"
              :rules="roomNameRules"
              autocomplete="off"
              :hint="!data.name ? '... using random name' : ''"
              persistent-hint
            ></v-text-field>
          </v-col>
          <v-col class="pb-0">
            <v-select
              label="Neko image"
              :items="nekoImages"
              v-model="data.neko_image"
            ></v-select>
          </v-col>
        </v-row>

        <template v-if="storageEnabled">
          <v-select v-model="data.profile_id" :items="profileOptions" label="Browser profile" class="browser-profile-select"
            :menu-props="{ maxWidth: 'min(552px, calc(100vw - 96px))', maxHeight: 360, contentClass: 'browser-profile-menu' }"
            hint="Exclusive binding lasts until deployment removal, even while stopped. Removing a deployment keeps its profile and Downloads."
            persistent-hint :disabled="profilesLoading">
            <template v-slot:selection="{ item }">
              <span class="profile-selection" :title="item.text">{{ item.profileName || item.text }}</span>
            </template>
            <template v-slot:item="{ item, attrs, on }">
              <v-list-item v-bind="attrs" v-on="on" class="profile-option">
                <v-list-item-content>
                  <div class="profile-option-name" :title="item.text">{{ item.profileName || item.text }}</div>
                  <div class="profile-option-detail">{{ item.binding || 'A new independent profile will be created.' }}</div>
                </v-list-item-content>
              </v-list-item>
            </template>
          </v-select>
          <v-text-field v-if="!data.profile_id" v-model="data.profile_name" label="New profile name (optional)"
            maxlength="120" hint="Defaults to the deployment name. A durable profile is always created." persistent-hint />
          <v-alert v-if="profilesError" type="error" dense outlined>{{ profilesError }}</v-alert>
        </template>

        <v-row align="center">
          <v-col>
            <v-text-field
              label="User password"
              v-model="data.user_pass"
              :append-icon="showUserPass ? 'mdi-eye' : 'mdi-eye-off'"
              :type="showUserPass ? 'text' : 'password'"
              @click:append="showUserPass = !showUserPass"
              autocomplete="off"
              :hint="!data.user_pass ? '... using random password' : ''"
              persistent-hint
            ></v-text-field>
          </v-col>
          <v-col>
            <v-text-field
              label="Admin password"
              v-model="data.admin_pass"
              :append-icon="showAdminPass ? 'mdi-eye' : 'mdi-eye-off'"
              :type="showAdminPass ? 'text' : 'password'"
              @click:append="showAdminPass = !showAdminPass"
              autocomplete="off"
              :hint="!data.admin_pass ? '... using random password' : ''"
              persistent-hint
            ></v-text-field>
          </v-col>
        </v-row>

        <!-- if uses mux vs. old logic -->
        <v-row align="center" v-if="usesMux">
          <v-col class="pt-0">
            <v-checkbox
              v-model="data.control_protection"
              label="Enable control protection"
              hide-details
              class="shrink ml-2 mt-0"
            ></v-checkbox>
            <div style="margin-left: 41px;"><i>Users can gain control only if at least one admin is in the room.</i></div>
          </v-col>
          <v-col class="pt-0">
            <v-checkbox
              v-model="data.implicit_control"
              label="Enable implicit control"
              hide-details
              class="shrink ml-2 mt-0"
            ></v-checkbox>
            <div style="margin-left: 41px;"><i>Users do not need to request control prior usage.</i></div>
          </v-col>
        </v-row>
        <v-row align="center" v-else>
          <v-col class="pt-0">
            <v-text-field
              label="Max connections"
              type="number"
              :rules="[ rules.required, rules.nonZero, rules.onlyPositive ]"
              v-model="data.max_connections"
            ></v-text-field>
          </v-col>
          <v-col class="pt-0">
            <v-checkbox
              v-model="data.control_protection"
              label="Enable control protection"
              hide-details
              class="shrink ml-2 mt-0"
            ></v-checkbox>
            <div style="margin-left: 41px;"><i>Users can gain control only if at least one admin is in the room.</i></div>
            <v-checkbox
              v-model="data.implicit_control"
              label="Enable implicit control"
              hide-details
              class="shrink ml-2 mt-0"
            ></v-checkbox>
            <div style="margin-left: 41px;"><i>Users do not need to request control prior usage.</i></div>
          </v-col>
        </v-row>

        <div class="my-3">
          <a @click="screen = !screen"> {{ screen ? 'Hide' : 'View' }} screen settings </a>
        </div>

        <v-row align="center" v-if="screen">
          <v-col>
            <v-select
              label="Initial screen configuration"
              :items="availableScreens"
              v-model="data.screen"
            ></v-select>
          </v-col>
          <v-col>
            <v-row align="center" no-gutters>
              <v-text-field
                :disabled="!maxFpsEnabled"
                :rules="[ rules.required, rules.nonZero, rules.onlyPositive ]"
                label="Max frames per second"
                v-model="data.video_max_fps"
              ></v-text-field>
              <v-checkbox
                v-model="maxFpsEnabled"
                hide-details
                class="shrink ml-2 mt-0"
              ></v-checkbox>
            </v-row>
          </v-col>
        </v-row>

        <div class="my-3">
          <a @click="extended = !extended"> {{ extended ? 'Hide' : 'View' }} extended settings </a>
        </div>

        <template v-if="extended">
          <v-row align="center">
            <v-col>
              <v-select
                label="Video codec"
                :items="videoCodecs"
                v-model="data.video_codec"
              ></v-select>
            </v-col>
            <v-col>
              <v-text-field
                label="Video bitrate"
                type="number"
                :rules="[ rules.required, rules.nonZero, rules.onlyPositive ]"
                v-model="data.video_bitrate"
              ></v-text-field>
            </v-col>
          </v-row>
          <v-row align="center">
            <v-col>
              <v-select
                label="Audio codec"
                :items="audioCodecs"
                v-model="data.audio_codec"
              ></v-select>
            </v-col>
            <v-col>
              <v-text-field
                label="Audio bitrate"
                type="number"
                :rules="[ rules.required, rules.nonZero, rules.onlyPositive ]"
                v-model="data.audio_bitrate"
              ></v-text-field>
            </v-col>
          </v-row>
        </template>

        <div class="my-3">
          <a @click="expert = !expert"> {{ expert ? 'Hide' : 'View' }} expert settings </a>
        </div>

        <template v-if="expert">
          <v-row align="center" no-gutters>
            <v-checkbox
              v-model="videoPipelineEnabled"
              hide-details
              class="shrink mr-2 mt-0"
            ></v-checkbox>
            <v-textarea
              :disabled="!videoPipelineEnabled"
              label="Video pipeline"
              :rows="1"
              auto-grow
              v-model="data.video_pipeline"
            ></v-textarea>
          </v-row>
          <v-row align="center" no-gutters>
            <v-checkbox
              v-model="audioPipelineEnabled"
              hide-details
              class="shrink mr-2 mt-0"
            ></v-checkbox>
            <v-textarea
              :disabled="!audioPipelineEnabled"
              label="Audio pipeline"
              :rows="1"
              auto-grow
              v-model="data.audio_pipeline"
            ></v-textarea>
          </v-row>
          <v-row align="center" no-gutters>
            <v-checkbox
              v-model="broadcastPipelineEnabled"
              hide-details
              class="shrink mr-2 mt-0"
            ></v-checkbox>
            <v-textarea
              :disabled="!broadcastPipelineEnabled"
              label="Broadcast pipeline"
              :rows="1"
              auto-grow
              v-model="data.broadcast_pipeline"
            ></v-textarea>
          </v-row>
          <v-row align="center" no-gutters>
            <h2> Environment variables </h2>
            <v-btn @click="addEnv" icon color="green"><v-icon>mdi-plus</v-icon></v-btn>
          </v-row>
          <v-row align="center" v-for="({ key, val }, index) in envList" :key="'env-'+index">
            <v-col class="py-0">
              <v-text-field
                label="Key"
                :value="key"
                @input="setEnv(index, { key: $event, val })"
                autocomplete="off"
              ></v-text-field>
            </v-col>
            <div> = </div>
            <v-col class="py-0">
              <v-text-field
                label="Value"
                :value="val"
                @input="setEnv(index, { key, val: $event })"
                autocomplete="off"
              ></v-text-field>
            </v-col>
            <div>
              <v-btn @click="delEnv(index)" icon color="red"><v-icon>mdi-close</v-icon></v-btn>
            </div>
          </v-row>
          <v-row align="center" no-gutters class="my-3">
              <h2> Mounts </h2>
              <v-btn @click="addMount" icon color="green"><v-icon>mdi-plus</v-icon></v-btn>
          </v-row>
          <v-row align="center" class="mb-2" v-for="({ type, host_path, container_path }, index) in data.mounts" :key="'mount-'+index">
            <v-col class="py-0" cols="2">
              <v-select
                label="Type"
                :items="mountTypes"
                :value="type"
                @input="setMount(index, { type: $event, host_path, container_path })"
              ></v-select>
            </v-col>
            <v-col class="py-0 pl-0">
              <v-text-field
                label="Host path"
                :value="host_path"
                @input="setMount(index, { type, host_path: $event, container_path })"
                :rules="[ rules.absolutePath ]"
                autocomplete="off"
              ></v-text-field>
            </v-col>
            <div> : </div>
            <v-col class="py-0">
              <v-text-field
                label="Container path"
                :value="container_path"
                @input="setMount(index, { type, host_path, container_path: $event})"
                :rules="[ rules.absolutePath ]"
                autocomplete="off"
              ></v-text-field>
            </v-col>
            <div>
              <v-btn @click="delMount(index)" icon color="red"><v-icon>mdi-close</v-icon></v-btn>
            </div>
          </v-row>
          <v-row align="center" no-gutters v-if="data.mounts && data.mounts.length > 0">
            <v-alert
              border="left"
              type="warning"
              v-if="!storageEnabled"
              style="width: 100%"
            >
              <p><strong>Not all mount types are available!</strong></p>
              <p class="mb-0">Private and Template mounts are not available, because storage is not enabled.</p>
            </v-alert>
            <p>
              <span :style="{ opacity: !storageEnabled ? 0.25 : 1 }">
                <strong>Private</strong>: Host path is relative to <code class="mx-1">&lt;selected profile storage root&gt;/</code>. <br />
                <strong>Template</strong>: Host path is relative to <code class="mx-1">&lt;storage path&gt;/templates/</code>, and will be readonly. <br />
              </span>
              <strong>Protected</strong>: Host path must be whitelisted in config and exists on the host, will be readonly. <br />
              <strong>Public</strong>: Host path must be whitelisted in config and exists on the host.
            </p>
          </v-row>
          <v-row align="center" no-gutters>
            <h2> Resources </h2>
          </v-row>
          <v-row align="center" v-if="data.resources">
            <v-col>
              <v-slider
                v-model="data.resources.memory"
                label="Memory"
                thumb-label="always"
                :thumb-size="30"
                :min="0"
                :max="8*1e9"
                :step="0.2*1e9"
                color="blue"
                hide-details
              >
                <template v-slot:thumb-label="{ value }">
                  <span v-if="value">{{ value | memory }}</span>
                  <span v-else>N/A</span>
                </template>
              </v-slider>
            </v-col>
            <v-col>
              <v-slider
                v-model="data.resources.nano_cpus"
                label="CPUs"
                thumb-label="always"
                :thumb-size="30"
                :min="0"
                :max="8*1e9"
                :step="0.2*1e9"
                color="blue"
                hide-details
              >
                <template v-slot:thumb-label="{ value }">
                  <span v-if="value">{{ value | nanocpus }}</span>
                  <span v-else>N/A</span>
                </template>
              </v-slider>
            </v-col>
          </v-row>
          <v-row align="center" v-if="data.resources">
            <v-col>
              <v-slider
                v-model="data.resources.shm_size"
                label="Shared memory"
                thumb-label="always"
                :thumb-size="30"
                :min="0"
                :max="20*1e9"
                :step="0.2*1e9"
                color="blue"
                hide-details
              >
                <template v-slot:thumb-label="{ value }">
                  <span v-if="value">{{ value | memory }}</span>
                  <span v-else>N/A</span>
                </template>
              </v-slider>
            </v-col>
            <v-col>
              <v-checkbox
                @change="$set(data.resources, 'gpus', $event ? ['all'] : [])"
                label="Enable Nvidia GPU support"
                hide-details
                class="shrink ml-2 mt-0"
              ></v-checkbox>
            <div style="margin-left: 41px;"><i>Nvidia docker runtime is required.</i></div>
            </v-col>
          </v-row>
          <v-row align="center" v-if="data.resources">
            <v-col class="pt-0">
              <v-combobox class="pt-0"
                label="Devices"
                v-model="data.resources.devices"
                :items="[ '/dev/dri/renderD128' ]"
                multiple
              >
                <template v-slot:selection="{ attrs, item, parent, selected }">
                  <v-chip
                    v-bind="attrs"
                    :input-value="selected"
                    label
                    small
                  >
                    <span class="pr-2">
                      {{ item }}
                    </span>
                    <v-icon
                      small
                      @click="parent.selectItem(item)"
                    >
                      $delete
                    </v-icon>
                  </v-chip>
                </template>
              </v-combobox>
            </v-col>
          </v-row>
          <v-row align="center" no-gutters class="mt-3">
            <h2> Network </h2>
          </v-row>
          <v-row align="center" class="mt-0">
            <v-col class="py-0">
              <v-text-field
                label="Hostname"
                v-model="data.hostname"
                autocomplete="off"
              ></v-text-field>
            </v-col>
            <v-col class="pt-0">
              <v-combobox class="pt-0"
                label="DNS"
                v-model="data.dns"
                multiple
                hide-details
              >
                <template v-slot:selection="{ attrs, item, parent, selected }">
                  <v-chip
                    v-bind="attrs"
                    :input-value="selected"
                    label
                    small
                  >
                    <span class="pr-2">
                      {{ item }}
                    </span>
                    <v-icon
                      small
                      @click="parent.selectItem(item)"
                    >
                      $delete
                    </v-icon>
                  </v-chip>
                </template>
              </v-combobox>
            </v-col>
          </v-row>
          <v-row align="center" no-gutters class="mb-3">
            <v-col>
              <p> <i> Leave empty to use container name. </i> </p>
            </v-col>
            <v-col class="pl-6">
              <p> <i> Leave empty to use system defaults.</i> </p>
            </v-col>
          </v-row>
        </template>

        <v-row align="center" no-gutters class="mt-3">
          <h2 class="my-3">
            Browser policy
          </h2>
          <v-checkbox
            v-model="browserPolicyEnabled"
            :disabled="!browserPolicyConfig || !storageEnabled"
            hide-details
            class="shrink ml-2 mt-0"
          ></v-checkbox>
        </v-row>

        <v-alert
          border="left"
          type="warning"
          v-if="!storageEnabled"
        >
          <p><strong>Not available!</strong></p>
          <p class="mb-0">Browser policy is not available, because storage is not enabled.</p>
        </v-alert>
        <template v-else-if="browserPolicyConfig">
          <v-row align="center" no-gutters class="mt-0">
            <v-col>
              <v-select
                v-model="browserPolicyContent.extensions"
                label="Extensions"
                :items="browserPolicyExtensions"
                multiple
                :disabled="!browserPolicyEnabled"
              ></v-select>
            </v-col>
          </v-row>

          <v-row align="center">
            <v-col>
              <v-checkbox
                v-model="browserPolicyContent.developer_tools"
                label="Enable developer tools"
                hide-details
                class="shrink ml-2 mt-0"
                :disabled="!browserPolicyEnabled"
              ></v-checkbox>
            </v-col>
            <v-col>
              <p>Browser data is retained in the selected durable profile.</p>
            </v-col>
          </v-row>
        </template>
        <v-alert
          border="left"
          type="info"
          v-else
        >
          <p><strong>Not available!</strong></p>
          <p class="mb-0">Browser policy is not available for this image.</p>
        </v-alert>
      </v-form>
    </v-card-text>
    <v-card-actions>
      <v-spacer></v-spacer>
      <v-btn
        color="gray darken-1"
        text
        @click="Close"
      >
        Close
      </v-btn>
      <v-btn
        color="blue"
        dark
        @click="Create(false)"
        :loading="loading && !loadingWithStart"
      >
        Create
      </v-btn>
      <v-btn
        color="green"
        dark
        @click="CreateAndStart()"
        :loading="loadingWithStart"
      >
        Create and start
      </v-btn>
    </v-card-actions>
  </v-card>
</template>

<script lang="ts">
import { Vue, Component, Ref } from 'vue-property-decorator'
import { AxiosError } from 'axios'
import { randomPassword } from '@/utils/random'
import { roomNameRules } from '@/utils/room-name'

import {
  RoomSettings,
  BrowserProfile,
  BrowserPolicyContent,
  BrowserPolicyExtension,
  BrowserPolicyTypeEnum,
  RoomMount,
  RoomMountTypeEnum,
} from '@/api/index'

export type VForm = Vue & {
  validate: () => boolean;
  resetValidation: () => boolean;
  reset: () => void;
}

@Component
export default class RoomsCreate extends Vue {
  @Ref('form') readonly _form!: VForm

  public profilesLoading = false
  public profilesError = ''
  public valid = true
  public roomNameRules = roomNameRules

  public screen = true
  public extended = false
  public expert = false

  public showUserPass = false
  public showAdminPass = false

  public maxFpsEnabled = true
  public videoPipelineEnabled = false
  public audioPipelineEnabled = false
  public broadcastPipelineEnabled = false
  public browserPolicyEnabled = false

  public loading = false
  public loadingWithStart = false
  public data: RoomSettings = JSON.parse(JSON.stringify(this.$store.state.defaultRoomSettings))
  public browserPolicyContent: BrowserPolicyContent = JSON.parse(JSON.stringify(this.$store.state.defaultBrowserPolicyContent))
  public envList: { key: string; val: string }[] = []

  // eslint-disable-next-line
  public rules: any = {
    // eslint-disable-next-line
    required(val: any) {
      return val === null || typeof val === 'undefined' || val === "" ? 'This filed is mandatory.' : true
    },
    onlyPositive(val: number) {
      return val < 0 ? 'Value cannot be negative.' : true
    },
    nonZero(val: string) {
      return val === "0" ? 'Value cannot be zero.' : true
    },
    absolutePath(val: string) {
      return val[0] !== "/" ? 'Must be absolute path, starting with /.' : true
    }
  }

  get nekoImages() {
    return this.$store.state.roomsConfig.neko_images
  }

  get storageEnabled() {
    return this.$store.state.roomsConfig.storage_enabled
  }

  get usesMux() {
    return this.$store.state.roomsConfig.uses_mux
  }

  get videoCodecs() {
    return this.$store.state.videoCodecs
  }

  get audioCodecs() {
    return this.$store.state.audioCodecs
  }

  get availableScreens() {
    return this.$store.state.availableScreens
  }

  get mountTypes() {
    return [
      ...(this.storageEnabled ? [
        {
          text: 'Private',
          value: 'private',
        },
        {
          text: 'Template',
          value: 'template',
        },
      ] : []),
      {
        text: 'Protected',
        value: 'protected',
      },
      {
        text: 'Public',
        value: 'public',
      },
    ]
  }

  get browserPolicyConfig() {
    const nekoImage = this.data.neko_image
    if (!nekoImage) return undefined

    return this.$store.state.browserPolicyConfig.find(
      // find browser policy config for this image
      ({ images }: { images: string[] }): boolean => images.some(
        // if nekoImage includes the image name
        (image: string): boolean => nekoImage.includes(image)
      )
    )
  }

  get browserPolicyExtensions() {
    const config = this.browserPolicyConfig
    if (!config) return []
  
    return this.$store.state.browserPolicyExtensions.map(({ text, value: values }: {
      text: string;
      value: Record<BrowserPolicyTypeEnum, BrowserPolicyExtension>;
    }) => {
      const value = values[config.type as BrowserPolicyTypeEnum]
      if (!value) return undefined
      return { text, value }
    })
  }

  get profileOptions() {
    return [
      { text: 'Create a new durable profile', value: '' },
      ...this.$store.state.profiles.map((profile: BrowserProfile) => ({
        text: `${profile.name} (${profile.id})${profile.room_id ? ' — bound to ' + profile.room_name : ''}`,
        profileName: profile.name,
        binding: profile.room_id ? `Bound to ${profile.room_name}` : 'Available',
        value: profile.id, disabled: !!profile.room_id,
      })),
    ]
  }

  addEnv() {
    Vue.set(this, 'envList', [ ...this.envList, { key: '', val: '' } ])
  }

  setEnv(index: number, data: { key: string; val: string }) {
    Vue.set(this.envList, index, data)
  }

  delEnv(index: number) {
    Vue.delete(this.envList, index)
  }

  addMount() {
    const type = this.storageEnabled ? RoomMountTypeEnum.private : RoomMountTypeEnum.public
    Vue.set(this.data, 'mounts', [ ...(this.data.mounts || []), { type, host_path: '', container_path: '' } ])
  }

  setMount(index: number, data: RoomMount) {
    let mounts = this.data.mounts || []
    if (index >= mounts.length) {
      mounts = [ ...mounts, data ]
    } else {
      mounts[index] = data
    }
    Vue.set(this.data, 'mounts', [ ...mounts])
  }

  delMount(index: number) {
    const mounts = this.data.mounts || []
    mounts.splice(index, 1)
    Vue.set(this.data, 'mounts', [ ...mounts])
  }

  async CreateAndStart() {
    this.loadingWithStart = true
    try {
      await this.Create(true)
    } finally {
      this.loadingWithStart = false
    }
  }

  async Create(start = false) {
    const valid = this._form.validate()
    if (!valid) return

    this.loading = true

    try {
      const envs = this.envList.reduce((obj, { key, val }) => ({ ...obj, [key]: val, }), {})

      await this.$store.dispatch(start ? 'ROOMS_CREATE_AND_START' : 'ROOMS_CREATE', {
        ...this.data,
        // eslint-disable-next-line
        user_pass: this.data.user_pass || randomPassword(),
        // eslint-disable-next-line
        admin_pass: this.data.admin_pass || randomPassword(),
        // eslint-disable-next-line
        max_connections: Number(this.data.max_connections), // ignored when uses mux
        // eslint-disable-next-line
        control_protection: Boolean(this.data.control_protection),
        // eslint-disable-next-line
        video_bitrate: Number(this.data.video_bitrate),
        // eslint-disable-next-line
        video_pipeline: this.videoPipelineEnabled ? this.data.video_pipeline : '',
        // eslint-disable-next-line
        video_max_fps: this.maxFpsEnabled ? Number(this.data.video_max_fps) : 0,
        // eslint-disable-next-line
        audio_bitrate: Number(this.data.audio_bitrate),
        // eslint-disable-next-line
        audio_pipeline: this.audioPipelineEnabled ? this.data.audio_pipeline : '',
        // eslint-disable-next-line
        broadcast_pipeline: this.broadcastPipelineEnabled ? this.data.broadcast_pipeline : '',
        envs,
        // eslint-disable-next-line
        browser_policy: this.browserPolicyEnabled && this.browserPolicyConfig ? {
          type: this.browserPolicyConfig.type,
          path: this.browserPolicyConfig.path,
          content: this.browserPolicyContent
        } : undefined,
      })
      this.Clear()
      this.$emit('finished', true)
     } catch(e) {
      const response = (e as AxiosError).response
      if (response) {
        this.$swal({
          title: 'Server error',
          text: String(response.data),
          icon: 'error',
        })
      } else {
        this.$swal({
          title: 'Network error',
          text: String(e),
          icon: 'error',
        })
      }
    } finally {
      this.loading = false
    }
  }

  Clear() {
    this._form.resetValidation()
    this.data = {
      ...JSON.parse(JSON.stringify(this.$store.state.defaultRoomSettings)),
      // eslint-disable-next-line
      neko_image: this.nekoImages[0],
    }
    this.browserPolicyContent = JSON.parse(JSON.stringify(this.$store.state.defaultBrowserPolicyContent))
    this.envList = Object.entries({...this.data.envs}).map(([ key, val ]) => ({ key, val, }))
  }

  Close() {
    this.Clear()
    this.$emit('finished', true)
  }

  async mounted() {
    this.Clear()
    if (!this.storageEnabled) return
    this.profilesLoading = true
    try { await this.$store.dispatch('PROFILES_LOAD') }
    catch { this.profilesError = 'Could not load profiles. Close and reopen this dialog to retry.' }
    finally { this.profilesLoading = false }
  }
}
</script>

<style scoped>
.browser-profile-select { margin-top: 24px; }
.profile-selection { min-width: 0; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.profile-option { min-width: 0; }
.profile-option-name { white-space: nowrap; line-height: 1.4; overflow: hidden; text-overflow: ellipsis; font-weight: 500; }
.profile-option-detail { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; font-size: 12px; line-height: 1.4; opacity: .75; margin-top: 3px; }
</style>
