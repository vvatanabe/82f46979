package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/vvatanabe/go82f46979/model"

	"github.com/vvatanabe/go82f46979/constant"

	"github.com/vvatanabe/go82f46979/sdk"
)

const needAWSMessage = "     Need first to run 'aws' command"

func Run() {

	fmt.Println("===========================================================")
	fmt.Println(">> Welcome to Priority Queueing CLI Tool!")
	fmt.Println("===========================================================")
	fmt.Println(" for help, enter one of the following: ? or h or help")
	fmt.Println(" all commands in CLIs need to be typed in lowercase")

	executionPath, _ := os.Getwd()
	fmt.Printf(" current directory is: [%s]\n", executionPath)

	region := flag.String("region", constant.AwsRegionDefault, "AWS region")
	credentialsProfile := flag.String("profile", constant.AwsProfileDefault, "AWS credentials profile")

	flag.Parse()

	fmt.Printf(" profile is: [%s]\n", *credentialsProfile)
	fmt.Printf(" region is: [%s]\n", *region)

	client := sdk.NewBuilder().
		WithRegion(*region).
		WithCredentialsProfileName(*credentialsProfile).
		Build()

	// 1. Create a Scanner using the InputStream available.
	scanner := bufio.NewScanner(os.Stdin)

	for {
		var shipment *model.Shipment

		// 2. Don't forget to prompt the user
		if shipment != nil {
			fmt.Printf("\nID <%s> >> Enter command: ", shipment.ID)
		} else {
			fmt.Print("\n >> Enter command: ")
		}

		// 3. Use the Scanner to read a line of text from the user.
		scanned := scanner.Scan()
		if !scanned {
			break
		}

		input := scanner.Text()
		if input == "" {
			continue
		}

		input = strings.TrimSpace(input)
		arr := strings.Split(input, " ")
		if len(arr) == 0 {
			continue
		}

		command := strings.ToLower(arr[0])
		var params []string = nil
		if len(arr) > 1 {
			params = make([]string, len(arr)-1)
			for i := 1; i < len(arr); i++ {
				params[i-1] = strings.TrimSpace(arr[i])
			}
		}

		// 4. Now, you can do anything with the input string that you need to.
		// Like, output it to the user.

		switch command {
		case "quit", "q":
			return
		case "h", "?", "help":
			fmt.Println("  ... this is CLI HELP!")
			fmt.Println("    > aws <profile> [<region>]                      [Establish connection with AWS; Default profile name: `default` and region: `us-east-1`]")
			fmt.Println("    > qstat | qstats                                [Retrieves the Queue statistics (no need to be in App mode)]")
			fmt.Println("    > dlq                                           [Retrieves the Dead Letter Queue (DLQ) statistics]")
			fmt.Println("    > create-test | ct                              [Create test Shipment records in DynamoDB: A-101, A-202, A-303 and A-404; if already exists, it will overwrite it]")
			fmt.Println("    > purge                                         [It will remove all test data from DynamoDB]")
			fmt.Println("    > ls                                            [List all shipment IDs ... max 10 elements]")
			fmt.Println("    > id <id>                                       [Get the application object from DynamoDB by app domain ID; CLI is in the app mode, from that point on]")
			fmt.Println("      > sys                                         [Show system info data in a JSON format]")
			fmt.Println("      > data                                        [Print the data as JSON for the current shipment record]")
			fmt.Println("      > info                                        [Print all info regarding Shipment record: system_info and data as JSON]")
			fmt.Println("      > update <new Shipment status>                [Update Shipment status .. e.g.: from UNDER_CONSTRUCTION to READY_TO_SHIP]")
			fmt.Println("      > reset                                       [Reset the system info of the current shipment record]")
			fmt.Println("      > ready                                       [Make the record ready for the shipment]")
			fmt.Println("      > enqueue | en                                [Enqueue current ID]")
			fmt.Println("      > peek                                        [Peek the Shipment from the Queue .. it will replace the current ID with the peeked one]")
			fmt.Println("      > done                                        [Simulate successful record processing completion ... remove from the queue]")
			fmt.Println("      > fail                                        [Simulate failed record's processing ... put back to the queue; needs to be peeked again]")
			fmt.Println("      > invalid                                     [Remove record from the regular queue to dead letter queue (DLQ) for manual fix]")
			fmt.Println("    > id")
		case "aws":
		case "id":
			if params == nil || len(params) == 0 {
				shipment = nil
				fmt.Println("     Going back to standard CLI mode!")
				continue
			}

			if client == nil {
				fmt.Println(needAWSMessage)
			} else {
				//id := params[0]
				//shipment, err := client.Get(context.Background(), id)
				//if err != nil {
				//	fmt.Println(err)
				//	os.Exit(1)
				//}
				//
				//dump, err := json.Marshal(shipment)
				//if err != nil {
				//	fmt.Println(err)
				//	os.Exit(1)
				//}
				//fmt.Printf("     Shipment's [%s] record dump\n%s", id, dump) // Replace "Utils.toJSON(shipment)" with actual JSON conversion function.
			}
		}
	}
}