package main

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

const (
	JobSucceeded = iota
	JobFailed
	JobCanceled
	JobRunning
)

const (
	AbortEmbed   = "https://media.giphy.com/media/G7iGNzr3VBING/giphy.gif?cid=790b7611zrca4oc8r9timt6d1fynumfr9k5wryshsup45uun&ep=v1_gifs_search&rid=giphy.gif&ct=g"
	SuccessEmbed = "https://media.giphy.com/media/sVnKj2wDhUTsFKFWhx/giphy.gif?cid=ecf05e472cewcrbacimrrrwkmjqzjfo6hiff91p1jl5x7gmv&ep=v1_gifs_search&rid=giphy.gif&ct=g"
	StartedEmbed = "https://media.giphy.com/media/A5ugHVbuFL3uo/giphy.gif?cid=790b7611h5fxvu46dd1n68atnngfes7rln3ctxbk0iaq05bf&ep=v1_gifs_search&rid=giphy.gif&ct=g"
	FailedEmbed  = "https://media.giphy.com/media/vVZypcXdxD508UOjfY/giphy.gif?cid=790b7611xvurpnd9l9fwhklhp25yzcvyen9vwwa231p13987&ep=v1_gifs_search&rid=giphy.gif&ct=g"
)

type messageComponentsBuilder struct {
	components []discordgo.MessageComponent
}

func NewMessageComponentsBuilder() *messageComponentsBuilder {
	return &messageComponentsBuilder{
		components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{},
			},
		},
	}
}

func (b *messageComponentsBuilder) AddLogsButton(u string) *messageComponentsBuilder {
	row := b.components[0].(discordgo.ActionsRow)
	row.Components = append(row.Components, discordgo.Button{
		Label: "Logs",
		Style: discordgo.LinkButton,
		URL:   u,
	})
	b.components[0] = row

	return b
}

func (b *messageComponentsBuilder) AddStatusButton(status int) *messageComponentsBuilder {
	var (
		label string
		style discordgo.ButtonStyle
	)

	switch status {
	case JobSucceeded:
		label = "Completed"
		style = discordgo.SuccessButton
	case JobFailed:
		label = "Failed"
		style = discordgo.DangerButton
	case JobCanceled:
		label = "Canceled"
		style = discordgo.SecondaryButton
	case JobRunning:
		label = "Running"
		style = discordgo.PrimaryButton
	default:
		label = "Unknown"
		style = discordgo.SecondaryButton
	}

	row := b.components[0].(discordgo.ActionsRow)
	row.Components = append(row.Components, discordgo.Button{
		Label:    label,
		CustomID: "status",
		Disabled: true,
		Style:    style,
	})
	b.components[0] = row

	return b
}

func (b *messageComponentsBuilder) AddActionButton(label, key string, enabled bool) *messageComponentsBuilder {
	row := b.components[0].(discordgo.ActionsRow)
	row.Components = append(row.Components, discordgo.Button{
		Label:    label,
		Style:    discordgo.DangerButton,
		CustomID: key,
		Disabled: !enabled,
	})
	b.components[0] = row

	return b
}

func (b *messageComponentsBuilder) Build() []discordgo.MessageComponent {
	return b.components
}

type embedBuilder struct {
	fields map[string]string
	embed  *discordgo.MessageEmbed
}

func NewEmbedBuilder() *embedBuilder {
	return &embedBuilder{
		embed: &discordgo.MessageEmbed{
			Type: discordgo.EmbedTypeLink,
		},
		fields: make(map[string]string),
	}
}

func (b *embedBuilder) ApplyStatus(status int) *embedBuilder {
	var (
		color int
		image string
	)

	switch status {
	case JobSucceeded:
		color = 0x92ad96
		image = SuccessEmbed
	case JobFailed:
		color = 0xff0000
		image = FailedEmbed
	case JobCanceled:
		color = 0x808080
		image = AbortEmbed
	case JobRunning:
		color = 0x0000ff
		image = StartedEmbed
	default:
		color = 0x808080
		image = FailedEmbed
	}

	b.embed.Color = color
	b.embed.Image = &discordgo.MessageEmbedImage{
		URL: image,
	}

	return b
}

func (b *embedBuilder) AddFields(fields map[string]string) *embedBuilder {
	for k, v := range fields {
		b.fields[k] = v
	}

	return b
}

func (b *embedBuilder) Build() *discordgo.MessageEmbed {
	var desc string

	for k, v := range b.fields {
		desc += fmt.Sprintf("**%s**: %s\n", k, v)
	}
	b.embed.Description = desc

	return b.embed
}
