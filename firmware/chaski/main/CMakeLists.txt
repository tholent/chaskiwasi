# main is the composition root and the one place `esp_*` is allowed
# (chaski-implementation-plan ground rule 3).
#
# The variant split lives here rather than behind a runtime flag: the bench
# control channel is COMPILED only into dev images, so there is nothing in a
# release binary to turn on. The condition is the console sitting on the
# USB-Serial-JTAG peripheral, which sdkconfig.dev sets and sdkconfig.defaults
# replaces with CONFIG_ESP_CONSOLE_NONE. That is not a stand-in for "dev": it is
# the peripheral the §14 bridge speaks over, and therefore the exact condition
# under which a USB transport and a control channel can exist at all.
set(chaski_srcs "app_main.cpp" "session.cpp" "wake.cpp" "chaski_strings.c")
set(chaski_dev_build 0)
if(CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG)
  list(APPEND chaski_srcs "bench_control.cpp")
  set(chaski_dev_build 1)
endif()

# REQUIRES is in two groups on purpose.
#
# The first is what main's own sources include: the stores and the engine it
# wires, the transport it hands them, pututu for the doorbell constants wake.cpp
# reads, cJSON for the control channel, NVS for provisioning, LittleFS for the
# letter partition, esp_timer for the monotonic clock.
#
# The second is every remaining component. main does not call them yet — panel,
# input, ui and the modem arrive in waves 3 and 4 — but MINIMAL_BUILD (see the
# project CMakeLists, F-C4) builds only `main` and its transitive requirements,
# so dropping them would mean the target build stopped compiling them under the
# cross-toolchain and `-Werror=all` entirely, and C-16's symbol scan would cover
# a smaller image than the product will ship. They are listed to keep the gate
# honest, and this comment is here so the list is not mistaken for cargo.
idf_component_register(SRCS ${chaski_srcs}
                       INCLUDE_DIRS "."
                       REQUIRES wire store ayllu syncengine transport pututu
                                json nvs_flash joltwallet__littlefs esp_timer
                                layout panel input power kipu ui modem)

target_compile_definitions(${COMPONENT_LIB} PUBLIC CHASKI_DEV_BUILD=${chaski_dev_build})
