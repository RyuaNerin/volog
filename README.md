# volog

- 디스코드 음성 채팅 입/퇴장을 채팅 채널에 기록해주는 봇

## [LICENSE](/LICENSE)

## How to use

1. [디스코드 개발자 포럼](https://discord.com/developers/applications)에서 APP을 생성합니다.

1. Bot을 생성한 후, 봇의 API 토큰을 발급받습니다.

1. 아래 텍스트의 `@@@@@@@@@@@@@@@@@@@@@@@` 부분을 생성된 애플리케이션의 ID로 변경하여 서버에 봇을 추가합니다.

    - 애플리케이션 ID는 **General Information**에서 확인할 수 있습니다.

    ```https://discord.com/api/oauth2/authorize?client_id=@@@@@@@@@@@@@@@@@@@@@@@&permissions=2048&scope=bot```

1. 아래 명령어를 통해 리포지토리를 복사한 후 컴파일합니다.

    ```bash
    > git clone https://github.com/RyuaNerin/volog
    > go build
    ```

1. [`config.example.json`](/config.example.json) 파일을 `config.json` 으로 복사 후 적절하게 수정합니다.

    ```bash
    > cp "config.example.json" "config.json"
    ```

    - 예시

        ```json
        {
            "bot_api"         : "@@@@@@@@@@@@@@@@@@@@@@@@@@",
            "guild_id"        : "111111111111111111",
            "text_channel_id" : "222222222222222222"
        }
        ```

1. 아래 명령어를 입력하여 실행 파일을 생성한 후, 실행합니다.

    ```bash
    > ./volog
    ```
