{% include 'kubernetes_suite.robot' %}

*** Settings ***
Library  {{ lookup('env', 'JANITOR_TEST_SOURCE') }}/test/integration/Janitor.py

*** Test Cases ***
Janitor Cleans Up Leftover OpenStack Resources
    [Tags]  janitor
    [Timeout]  45 minutes
    [Setup]  Prepare Janitor Fixtures
    [Teardown]  Remove Janitor Fixtures
    Preserve Shared Load Balancers And Volumes
    Delete Owned Resources And Credential
