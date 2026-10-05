package ses

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/warpstreamlabs/bento/public/service"
)

// sessionFields returns the same set of AWS session fields used by the
// built-in Bento AWS components. This keeps the credentials configuration
// consistent with the rest of Bento:
//
//	credentials:
//	  profile: ""
//	  id: ""
//	  secret: ""
//	  token: ""
//	  role: ""
//	  role_external_id: ""
//	  expiry_window: ""
func sessionFields() []*service.ConfigField {
	return []*service.ConfigField{
		service.NewStringField("region").
			Description("The AWS region to target.").
			Default("").
			Advanced(),
		service.NewStringField("endpoint").
			Description("Allows you to specify a custom endpoint for the AWS API.").
			Default("").
			Advanced(),
		service.NewObjectField("credentials",
			service.NewStringField("profile").
				Description("A profile from `~/.aws/credentials` to use.").
				Default(""),
			service.NewStringField("id").
				Description("The ID of credentials to use.").
				Default("").Advanced(),
			service.NewStringField("secret").
				Description("The secret for the credentials being used.").
				Default("").Advanced().Secret(),
			service.NewStringField("token").
				Description("The token for the credentials being used, required when using short term credentials.").
				Default("").Advanced(),
			service.NewBoolField("from_ec2_role").
				Description("Use the credentials of a host EC2 machine configured to assume [an IAM role associated with the instance](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_switch-role-ec2.html).").
				Default(false),
			service.NewStringField("role").
				Description("A role ARN to assume.").
				Default("").Advanced(),
			service.NewStringField("role_external_id").
				Description("An external ID to provide when assuming a role.").
				Default("").Advanced(),
			service.NewDurationField("expiry_window").
				Description("Allow the credentials to trigger refreshing prior to the credentials actually expiring. This is beneficial so race conditions with expiring credentials do not cause requests to fail. For example '10s' would refresh credentials ten seconds before expiration. Setting to a duration of `0` disables the expiry window.").
				Default("").Advanced()).
			Advanced().
			Description("Optional manual configuration of AWS credentials to use. More information can be found [in this document](/docs/guides/cloud/aws)."),
	}
}

func getCredentialsCacheOptions(conf *service.ParsedConfig) func(*aws.CredentialsCacheOptions) {
	expiryWindow, _ := conf.FieldDuration("expiry_window")
	return func(cco *aws.CredentialsCacheOptions) {
		cco.ExpiryWindow = expiryWindow
	}
}

// getSession loads an aws.Config using the parsed component fields. The logic
// mirrors the built-in Bento AWS components so that the credentials behaviour
// (profile, static keys, assumed roles, EC2 role, expiry window) is identical.
func getSession(ctx context.Context, parsedConf *service.ParsedConfig, opts ...func(*config.LoadOptions) error) (aws.Config, error) {
	if region, _ := parsedConf.FieldString("region"); region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	credsConf := parsedConf.Namespace("credentials")
	if profile, _ := credsConf.FieldString("profile"); profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	} else if id, _ := credsConf.FieldString("id"); id != "" {
		secret, _ := credsConf.FieldString("secret")
		token, _ := credsConf.FieldString("token")
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			id, secret, token,
		)))
	}

	credsCacheOpts := getCredentialsCacheOptions(credsConf)
	opts = append(opts, config.WithCredentialsCacheOptions(credsCacheOpts))

	conf, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return conf, err
	}

	if endpoint, _ := parsedConf.FieldString("endpoint"); endpoint != "" {
		conf.BaseEndpoint = &endpoint
	}

	if role, _ := credsConf.FieldString("role"); role != "" {
		stsSvc := sts.NewFromConfig(conf)

		var stsOpts []func(*stscreds.AssumeRoleOptions)
		if externalID, _ := credsConf.FieldString("role_external_id"); externalID != "" {
			stsOpts = append(stsOpts, func(aro *stscreds.AssumeRoleOptions) {
				aro.ExternalID = &externalID
			})
		}

		creds := stscreds.NewAssumeRoleProvider(stsSvc, role, stsOpts...)
		conf.Credentials = aws.NewCredentialsCache(creds, credsCacheOpts)
	}

	if useEC2, _ := credsConf.FieldBool("from_ec2_role"); useEC2 {
		conf.Credentials = aws.NewCredentialsCache(ec2rolecreds.New(), credsCacheOpts)
	}
	return conf, nil
}
