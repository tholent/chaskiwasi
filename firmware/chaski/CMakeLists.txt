# Chaski firmware — ESP-IDF project (target build).
#
# Host tests do NOT use this file: they build the same components through
# test/firmware/host/CMakeLists.txt with no IDF installed. That split is the
# point of implementation-plan ground rule 3, and it is what keeps waves 1-2
# moving without hardware.
cmake_minimum_required(VERSION 3.16)

set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_LIST_DIR}/components")

include($ENV{IDF_PATH}/tools/cmake/project.cmake)

# D-3, enforced by the build graph rather than by config.
#
# Setting CONFIG_ESP_WIFI_ENABLED=n in sdkconfig.defaults does NOT work: the
# symbol is derived from SOC_WIFI_SUPPORTED, so the generated sdkconfig came
# back =y and the build compiled esp_wifi and wifi_provisioning anyway (F-C4).
#
# MINIMAL_BUILD restricts the build to `main` and its transitive REQUIRES.
# Nothing in this firmware requires esp_wifi or bt, so neither is built at all —
# there is no radio stack to accidentally reach, in either build variant, which
# is what "not compiled in, not merely disabled" has to mean (client §1.1, B.2).
# C-16's ELF symbol scan is the check that keeps this honest.
idf_build_set_property(MINIMAL_BUILD ON)

project(chaski)
